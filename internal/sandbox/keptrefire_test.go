package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
)

// clockedGate is a [TurnGate] whose refusal a case decides from inside its
// bubble: refusing says, when it is asked, whether the gate still refuses and
// with what; the gate counts every resume it is asked about.
type clockedGate struct {
	mu       sync.Mutex
	asked    int
	refusing func() (Refusal, bool)
}

func (g *clockedGate) admit(ctx context.Context, _ string, _ ResumeRoute) (context.Context, func(), error) {
	g.mu.Lock()
	g.asked++
	g.mu.Unlock()
	if refusal, refused := g.refusing(); refused {
		return nil, nil, &TurnRefused{Refusal: refusal}
	}
	return ctx, func() {}, nil
}

func (g *clockedGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.asked
}

// A KEPT COMPLETION IS RE-FIRED WITHOUT A SIGNAL, AT ITS REFUSAL'S INSTANT OR AT
// THE BACKSTOP. A completion the node's gate refuses on a condition that clears
// by waiting is acknowledged and kept for the condition's signal, and the waiter
// that drives its box announces it again on its schedule. But some conditions
// lift on the clock with no edge anything signals: a seat's lease merely stale
// from a late renew, which the gate names one heartbeat on, and a provider's
// limit whose instant comes with no park to release. While the coordination
// store does not answer the host of the job's box, that waiter announces
// nothing (these rigs run no waiter at all), and on the node that keeps the
// seat, with no restart and no seat move to re-arm it from its record, the
// completion waited with no bound. So every keep arms a re-fire of its own: at
// the instant its refusal names, never later than [answerWaitBackstop], and at
// the backstop where it names none.
//
// Mutation: keep a refused completion without arming its re-fire, and none of
// these is ever carried.
func TestAKeptCompletionIsReFiredWithoutASignal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cond Condition
		// lifts is how long after the refusal the condition lifts, with
		// nothing to signal it; names says whether the refusal names that
		// instant ([Refusal.Until]); due is when the completion is to be
		// carried, its instant or the backstop; asks is how many times the
		// gate is asked by then, the delivery's and each re-fire's, no
		// re-fire later than the backstop.
		lifts time.Duration
		names bool
		due   time.Duration
		asks  int
	}{
		{name: "a seat's lease merely stale, renewed a heartbeat on", cond: "ownership",
			lifts: 15 * time.Second, names: true, due: 15 * time.Second, asks: 2},
		{name: "a provider's limit that lifts on its clock", cond: conditionLimit,
			lifts: 20 * time.Minute, names: true, due: 20 * time.Minute, asks: 5},
		{name: "a condition naming no instant, lifted unsignalled", cond: "pause",
			lifts: time.Minute, due: answerWaitBackstop, asks: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				rig := newCoordRigWith(t, func(o *CoordinatorOptions) { o.After = nil })
				lifts := time.Now().Add(tc.lifts)
				gate := &clockedGate{refusing: func() (Refusal, bool) {
					now := time.Now()
					if !now.Before(lifts) {
						return Refusal{}, false
					}
					refusal := Refusal{Condition: tc.cond, Reason: "the seat may not start the turn yet"}
					if tc.names {
						refusal.Until = rig.now.Add(lifts.Sub(now))
					}
					return refusal, true
				}}
				rig.coordinator.gate = gate.admit
				rig.launch("t1")
				rig.runner.Finish(wholeAccount())
				_, ev := rig.completion("t1")
				err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev)
				if controlResult(err).Outcome != queue.OutcomeAck {
					t.Fatalf("the premise: the refused delivery = %v; want it acknowledged and kept", err)
				}
				// A SECOND SHORT OF ITS DUE, the completion is still kept.
				time.Sleep(tc.due - time.Second)
				synctest.Wait()
				if resumed := rig.resumer.calls(); len(resumed) != 0 {
					t.Fatalf("the turn resumed %d times before its condition lifted", len(resumed))
				}
				// A SECOND PAST IT, with no signal and no announcement, it
				// is carried, once.
				time.Sleep(2 * time.Second)
				synctest.Wait()
				if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
					t.Fatalf("the turn resumed %d times (%+v) %v after the refusal, its condition lifted at %v "+
						"with nothing to signal it; want it carried once by the kept completion's own re-fire",
						len(resumed), resumed, tc.due+time.Second, tc.lifts)
				}
				if asked := gate.count(); asked != tc.asks {
					t.Fatalf("the gate was asked %d times by then; want %d, the delivery's and one re-fire at "+
						"the refusal's instant or each %v backstop before it", asked, tc.asks, answerWaitBackstop)
				}
			})
		})
	}
}

// A COMPLETION THIS NODE HOLDS IS RE-FIRED AT ITS INSTANT, AND NEVER SOONER. A
// resume refused after its claim, on a limit that names the instant it lifts,
// is held here until then ([Coordinator.stillRefused]): the gate admitted it
// and would again, so every re-fire before the instant claimed the job again,
// woke its box to collect it and ran its review into the same refusal. Its own
// re-fire is the keep's, and fired at the backstop it dropped the keep, the
// hold with it, so the re-fire found nothing held and resumed the job into the
// limit every five minutes until the instant. Armed for the instant, it comes
// once, when the limit lifts with nothing to signal it.
//
// Its timer fires at each backstop before the instant, and re-fires nothing
// there (TestAHeldCompletionPastItsDeadlineEndsWithinTheBackstop), so the case
// runs on the bubble's clock, which every one of those reads.
//
// Mutation: re-fire a held completion no later than the backstop, as the
// gate's own refusals are, and the job is resumed again five minutes on, into
// the same limit.
func TestACompletionHeldHereIsReFiredAtItsInstantAndNeverSooner(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := bubbleRig(t)
		const lifts = 4 * answerWaitBackstop
		limitedAfterClaim(t, rig, time.Now().Add(lifts))
		// PAST EVERY BACKSTOP, a second short of the instant: still held,
		// and nothing resumed into the limit again.
		time.Sleep(lifts - time.Second)
		synctest.Wait()
		if begun := rig.resumer.turnsBegun(); begun != 1 {
			t.Fatalf("%d resumed turns began before the limit's instant; want only the one that met it, "+
				"the job held here until %v", begun, lifts)
		}
		// THE LIMIT LIFTS AT ITS INSTANT, with nothing to signal it, and the
		// held completion is carried, once.
		rig.resumer.failWith(nil)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if calls, begun := rig.resumer.calls(), rig.resumer.turnsBegun(); len(calls) != 1 || begun != 2 {
			t.Fatalf("the held completion resumed %d times in %d turns a second past its instant; want it "+
				"carried once by its own re-fire", len(calls), begun)
		}
	})
}

// A RE-FIRE THE STORE DID NOT ANSWER IS KEPT FOR ITS BACKSTOP. A kept
// completion's re-fire, a recovery's at once from the job's record or a
// signal's, has no delivery the broker brings back when it fails, and the
// waiter that would announce the job again announces nothing while its store
// is silent. A recovery's re-arm whose re-fire failed before the claim was
// kept on the recorded condition, whose signal this holder may never see; a
// re-fire the gate admitted and whose claim the store did not answer was kept
// by nothing at all. Either way the completion waited, on the node that holds
// the seat, for an announcement that did not come. So both are kept with a
// re-fire of their own at the backstop.
//
// Mutation: keep a recovery's re-arm without its re-fire, or drop a re-fire's
// completion when its claim fails, and the completion is never carried.
func TestARefireTheStoreDidNotAnswerIsKeptForItsBackstop(t *testing.T) {
	t.Parallel()

	t.Run("a recovery's re-arm whose re-fire the gate could not ask", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		// NODE-B HOLDS THE SEAT, and the job's box is on node-a's host, whose
		// coordination store does not answer its read of the run's row: its
		// waiter announces nothing.
		host, _ := unsettledHost(t, rig)
		gate := &conditionGate{}
		gate.refuse(conditionLimit)
		rig.coordinator.gate = gate.admit
		_, ev := rig.completion("t1")
		if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); controlResult(err).Outcome != queue.OutcomeAck {
			t.Fatalf("the premise: the refused delivery = %v; want it acknowledged and recorded", err)
		}
		// THE NODE RESTARTS, and its gate cannot read the seat's lease from
		// the store when the recovery re-fires the recorded completion at
		// once; it can afterwards, and nothing signals that.
		var unanswered atomic.Bool
		unanswered.Store(true)
		next := &clockedGate{refusing: func() (Refusal, bool) { return Refusal{}, false }}
		failing := func(ctx context.Context, handle string, route ResumeRoute) (context.Context, func(), error) {
			if unanswered.Load() {
				return nil, nil, fmt.Errorf("node: reading the lease of seat %s: %w", handle, errStoreUnanswered)
			}
			return next.admit(ctx, handle, route)
		}
		rig.restart(t, rig.queue, "node-b", rigLease, failing)
		if err := rig.recoverSeat(t.Context(), rigLease.Owner, rigLease.Epoch); err != nil {
			t.Fatalf("recovering the seat: %v", err)
		}
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 0 {
			t.Fatalf("the premise: the turn resumed %d times while the gate could not ask", len(resumed))
		}
		// THE GATE CAN ASK AGAIN, and the host's store answers the collection,
		// its waiter having announced nothing.
		unanswered.Store(false)
		host.answer()
		// EVERY RE-FIRE ARMED, the restarted node's among them (the node
		// before the restart is the rig's, and holds no lease).
		rig.fireRetries()
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
			t.Fatalf("the turn resumed %d times (%+v) at the re-arm's backstop; want it carried once",
				len(resumed), resumed)
		}
	})

	t.Run("a re-fire whose claim the store did not answer", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		// THE STORE DOES NOT ANSWER THE FIRST CLAIM of the completion, the
		// re-fire's, and answers every one after it.
		rig.coordinator.pending = &unansweredClaims{PendingStore: rig.coordinator.pending, silent: 1,
			answered: func() {}}
		gate := &conditionGate{}
		gate.refuse(conditionLimit)
		rig.coordinator.gate = gate.admit
		rig.launch("t1")
		rig.runner.Finish(wholeAccount())
		_, ev := rig.completion("t1")
		if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); controlResult(err).Outcome != queue.OutcomeAck {
			t.Fatalf("the premise: the refused delivery = %v; want it acknowledged and kept", err)
		}
		// THE CONDITION CLEARS AND IS SIGNALLED, and the store does not
		// answer the re-fire's claim.
		gate.refuse("")
		rig.coordinator.Readmit(conditionLimit, "swe")
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 0 {
			t.Fatalf("the premise: the turn resumed %d times with its claim unanswered", len(resumed))
		}
		if delays := rig.retries.delays(); len(delays) != 1 || delays[0] != answerWaitBackstop {
			t.Fatalf("the failed re-fire's re-fires armed = %v; want one, at the %v backstop", delays,
				answerWaitBackstop)
		}
		rig.fireRetries()
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
			t.Fatalf("the turn resumed %d times (%+v) at the failed re-fire's backstop; want it carried once",
				len(resumed), resumed)
		}
	})
}

// A SIGNAL THAT ENDED A HOLD IS NOT UNDONE BY A RE-FIRE THE STORE DID NOT
// ANSWER. A completion held here until a limit's instant is re-fired at once
// when the signal that ends the limit comes, and a re-fire that settles nothing
// and was not refused again is kept again, for a re-fire of its own. Kept again
// as it was, held, it refused the job's announcements here once more and armed
// that re-fire for the instant the signal had ended, up to the hour a vendor's
// window is benched for. Kept unheld, its re-fire comes at the backstop, and
// the waiter's next announcement is asked of the gate.
//
// Mutation: keep the failed re-fire again with its hold, and the completion is
// held here until the instant, its re-fire armed for it.
func TestASignalledRefireTheStoreDidNotAnswerIsHeldNoLonger(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	// THE STORE ANSWERS THE FIRST CLAIM, whose resume met the limit, and not
	// the second, the signal's re-fire's.
	rig.coordinator.pending = &unansweredClaims{PendingStore: rig.coordinator.pending, before: 1, silent: 1,
		answered: func() {}}
	until := rig.now.Add(time.Hour)
	payload, _ := limitedAfterClaim(t, rig, until)
	// THE LIMIT LIFTS, AND THAT IS SIGNALLED; the re-fire's claim goes
	// unanswered.
	rig.resumer.failWith(nil)
	rig.coordinator.Readmit(conditionLimit, "swe")
	rig.coordinator.inflight.Wait()
	if calls := rig.resumer.calls(); len(calls) != 0 {
		t.Fatalf("the premise: the turn resumed %d times with the re-fire's claim unanswered", len(calls))
	}
	if refusal, held := rig.coordinator.stillRefused(payload); held {
		t.Fatalf("the completion is held here again until %v, after the signal that ended its hold; want it "+
			"kept unheld", refusal.Until)
	}
	if delays := rig.retries.delays(); len(delays) != 1 || delays[0] != answerWaitBackstop {
		t.Fatalf("the failed re-fire's re-fires armed = %v; want one, at the %v backstop", delays,
			answerWaitBackstop)
	}
	rig.fireRetries()
	rig.coordinator.inflight.Wait()
	if calls := rig.resumer.calls(); len(calls) != 1 {
		t.Fatalf("the turn resumed %d times at the failed re-fire's backstop; want it carried once", len(calls))
	}
}

// A KEPT COMPLETION'S RE-FIRE GOES WITH IT. The re-fire armed for a kept
// completion is the keep's own: once the completion goes, carried by an
// announcement the gate admitted, let go of with the seat, or kept afresh by a
// later refusal with a re-fire of its own, the armed one is cancelled, and a
// settled run is never re-fired.
//
// Mutation: leave the re-fire armed when its keep goes, and fire it on its own
// account whatever is kept, and the settled run is re-fired into the gate.
func TestAKeptCompletionsRefireGoesWithIt(t *testing.T) {
	t.Parallel()

	t.Run("carried by the waiter's next announcement", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		gate := &conditionGate{}
		gate.refuse(conditionLimit)
		rig.coordinator.gate = gate.admit
		rig.launch("t1")
		rig.runner.Finish(wholeAccount())
		_, ev := rig.completion("t1")
		if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); controlResult(err).Outcome != queue.OutcomeAck {
			t.Fatalf("the premise: the refused delivery = %v; want it acknowledged and kept", err)
		}
		if delays := rig.retries.delays(); len(delays) != 1 {
			t.Fatalf("the premise: the keep armed %v; want its one re-fire", delays)
		}
		// THE CONDITION LIFTS, and the waiter's next announcement carries the
		// completion.
		gate.refuse("")
		_, again := rig.completion("t1")
		if err := rig.coordinator.OnEvent(delivery(t.Context(), again), again); err != nil {
			t.Fatalf("the announcement after the condition lifted = %v; want it carried", err)
		}
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 {
			t.Fatalf("the premise: the turn resumed %d times; want it carried once", len(resumed))
		}
		asked := gate.count()
		if delays := rig.retries.delays(); len(delays) != 0 {
			t.Fatalf("the carried completion's re-fires still armed: %v; want them cancelled", delays)
		}
		rig.fireRetries()
		rig.coordinator.inflight.Wait()
		if now := gate.count(); now != asked {
			t.Fatalf("the gate was asked %d more times after the completion was carried; want its keep's "+
				"re-fire gone with it", now-asked)
		}
	})

	t.Run("let go of with the seat", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		gate := &conditionGate{}
		gate.refuse(conditionLimit)
		rig.coordinator.gate = gate.admit
		rig.launch("t1")
		rig.runner.Finish(wholeAccount())
		_, ev := rig.completion("t1")
		if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); controlResult(err).Outcome != queue.OutcomeAck {
			t.Fatalf("the premise: the refused delivery = %v; want it acknowledged and kept", err)
		}
		asked := gate.count()
		rig.coordinator.ReleaseSeat("swe")
		if delays := rig.retries.delays(); len(delays) != 0 {
			t.Fatalf("the released seat's re-fires still armed: %v; want them cancelled", delays)
		}
		rig.fireRetries()
		rig.coordinator.inflight.Wait()
		if now := gate.count(); now != asked {
			t.Fatalf("the gate was asked %d more times after the seat was let go of; want nothing re-fired",
				now-asked)
		}
	})

	t.Run("kept afresh by a later refusal", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		gate := &conditionGate{}
		gate.refuse(conditionLimit)
		rig.coordinator.gate = gate.admit
		rig.launch("t1")
		rig.runner.Finish(wholeAccount())
		_, ev := rig.completion("t1")
		for range 2 {
			if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); controlResult(err).Outcome != queue.OutcomeAck {
				t.Fatalf("the premise: the refused delivery = %v; want it acknowledged and kept", err)
			}
		}
		if delays := rig.retries.delays(); len(delays) != 1 {
			t.Fatalf("two keeps of one completion left %d re-fires armed (%v); want the later keep's alone",
				len(delays), delays)
		}
	})
}

// unliftedHolds is a run store whose coordination store does not answer a lift
// of a run's hold while silent is set, and answers every other call as the
// store beneath it does.
type unliftedHolds struct {
	PendingStore
	silent atomic.Bool
}

func (s *unliftedHolds) LiftHold(ctx context.Context, turnID string, cond Condition) (bool, error) {
	if s.silent.Load() {
		return false, fmt.Errorf("sandbox: lift the hold of run %s: %w", turnID, errStoreUnanswered)
	}
	return s.PendingStore.LiftHold(ctx, turnID, cond)
}

// signalledOnRecovery is a run store that runs signal once, as soon as a
// listing of one seat's runs has been answered, and answers every call as the
// store beneath it does: a signal that lands while a recovery of the seat is
// under way, after it read the seat's runs.
type signalledOnRecovery struct {
	PendingStore
	signal func()
	once   sync.Once
}

func (s *signalledOnRecovery) ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error) {
	runs, err := s.PendingStore.ListActiveForSeat(ctx, handle)
	s.once.Do(s.signal)
	return runs, err
}

// countedCalls is a run store that counts its reads of one run's row and its
// claims of a run for its resume, and answers every call as the store beneath
// it does.
type countedCalls struct {
	PendingStore
	gets, claims atomic.Int64
}

func (s *countedCalls) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	s.gets.Add(1)
	return s.PendingStore.Get(ctx, turnID)
}

func (s *countedCalls) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	s.claims.Add(1)
	return s.PendingStore.ClaimForResume(ctx, turnID, tail, fence)
}

// liftedOnRefusal is a run store that lifts a run's hold for the budget as soon
// as it has refused a claim of the run's completion, and answers every call as
// the store beneath it does: the budget's signal, whose lift of every hold on
// the condition lands between a claim the hold refused and what the claimant
// does next.
type liftedOnRefusal struct{ PendingStore }

func (s liftedOnRefusal) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	run, won, err := s.PendingStore.ClaimForResume(ctx, turnID, tail, fence)
	if err == nil && !won {
		_, _ = s.PendingStore.LiftHold(ctx, turnID, "budget")
	}
	return run, won, err
}

// unlistedRuns is a run store whose coordination store does not answer a
// listing of the runs, which is all a waiter announces from.
type unlistedRuns struct{ PendingStore }

func (unlistedRuns) ListActive(context.Context) ([]PendingRun, error) {
	return nil, fmt.Errorf("sandbox: list the runs: %w", errStoreUnanswered)
}

// liftAfterFence is a run store that orders a signal's lift of a hold after a
// recovery's fence of the run and before the recovery goes on: its listing of
// the runs (a signal's, which lifts every hold on its condition) waits for the
// recovery's read of the row after its fence (the first read once ownership
// was claimed), and that read returns only once a claim of the job has been
// asked, the lift's re-fire's. It answers every call as the store beneath it
// does.
type liftAfterFence struct {
	PendingStore
	fenced, claimed       chan struct{}
	owned, read, claiming atomic.Bool
}

func newLiftAfterFence(store PendingStore) *liftAfterFence {
	return &liftAfterFence{PendingStore: store, fenced: make(chan struct{}), claimed: make(chan struct{})}
}

func (s *liftAfterFence) ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error) {
	won, err := s.PendingStore.ClaimOwnership(ctx, turnID, owner, epoch)
	s.owned.Store(true)
	return won, err
}

func (s *liftAfterFence) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	run, found, err := s.PendingStore.Get(ctx, turnID)
	if s.owned.Load() && s.read.CompareAndSwap(false, true) {
		close(s.fenced)
		select {
		case <-s.claimed:
		case <-ctx.Done():
		}
	}
	return run, found, err
}

func (s *liftAfterFence) ListActive(ctx context.Context) ([]PendingRun, error) {
	select {
	case <-s.fenced:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.PendingStore.ListActive(ctx)
}

func (s *liftAfterFence) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	if s.claiming.CompareAndSwap(false, true) {
		close(s.claimed)
	}
	return s.PendingStore.ClaimForResume(ctx, turnID, tail, fence)
}

// liftAfterRecovery is a run store that orders a signal's lift of a hold after
// a recovery's own lift of it has been asked and before that lift is answered:
// its listing of the runs (a signal's) waits for recovered, and the first lift
// of a hold it is asked for is made only once a second has landed. It answers
// every call as the store beneath it does.
type liftAfterRecovery struct {
	PendingStore
	recovered <-chan struct{}
	second    chan struct{}
	lifts     atomic.Int64
}

func (s *liftAfterRecovery) ListActive(ctx context.Context) ([]PendingRun, error) {
	select {
	case <-s.recovered:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.PendingStore.ListActive(ctx)
}

func (s *liftAfterRecovery) LiftHold(ctx context.Context, turnID string, cond Condition) (bool, error) {
	n := s.lifts.Add(1)
	if n == 1 {
		select {
		case <-s.second:
		case <-ctx.Done():
		}
	}
	lifted, err := s.PendingStore.LiftHold(ctx, turnID, cond)
	if n == 2 {
		close(s.second)
	}
	return lifted, err
}

// budgetHeldOnRow is a rig whose swe run finished and whose resume its seat's
// budget held on the run's row until until, the completion's delivery
// acknowledged; and whose box's host's waiter reads the runs through a store
// that does not answer it, so it announces nothing, before the hold's instant
// or after it.
func budgetHeldOnRow(t *testing.T, rig *coordRig, until time.Time) {
	t.Helper()
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.resumer.failWith(budgetHeld(until))
	rig.runner.Finish(Result{Success: true, Text: "first pass"})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the premise: the held resume = %v; want its completion spent", err)
	}
	if got := rig.get("t1"); got.Status != StatusRunning || got.HeldFor != "budget" || !got.HeldUntil.Equal(until) {
		t.Fatalf("the premise: the row is %s held for %q until %v; want running, held until %v", got.Status,
			got.HeldFor, got.HeldUntil, until)
	}
	rig.resumer.failWith(nil)
	silent, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: unlistedRuns{rig.pending}, Manager: rig.managers, Drives: rigDrives(Direct),
		Now: func() time.Time { return until.Add(time.Second) },
	})
	if err != nil {
		t.Fatalf("NewWaiter: %v", err)
	}
	if fired, _ := silent.Tick(t.Context()); fired != 0 {
		t.Fatalf("the premise: the host's waiter announced %d completions through its silent store", fired)
	}
}

// A COMPLETION ITS SEAT'S BUDGET HELD ON ITS ROW IS KEPT FOR THE HOLD. A
// resume the budget refused once it ran hands its claim back holding the run's
// row until the window turns over, and its delivery is spent: the waiter that
// drives the job's box announces it again once the hold lifts. But that waiter
// reads the row through its host's coordination store, and before the instant
// nothing else carried the job: the claim refuses a held row, a signal only
// lifted the hold, and a recovery re-armed only an owed refusal. So with that
// store silent, no restart and no seat move, the job waited with no bound. It
// is kept on this node, unheld: re-fired at the hold's instant and never
// sooner, since before it the claim takes nothing; and on its condition's
// signal once the lift of the hold has landed, and not before, since a re-fire
// beside the lift met the hold still standing and dropped the keep; and every
// lift its signal makes re-fires the job, unless this node carries it already,
// kept or a re-fire of it on its way, so it is re-fired once. A claim the
// hold refuses leaves it kept: a delivery queued behind the resume that held
// the job leaves it standing, and the re-fire of whatever was kept in its place
// keeps it again from the row.
//
// Mutation: keep nothing for a hold on the row, and the job is never carried;
// arm its re-fire no later than the backstop, and it is armed sooner than the
// instant; re-fire it on its signal without lifting the hold first, and its
// claim meets the hold; re-fire it after a lift the store did not answer, its
// signal's or a stop's, and the job is claimed into the hold before its
// instant; drop it as every keep is dropped once a delivery is admitted, and a
// delivery the hold refuses reads the row to keep it again; leave it standing
// once a claim of it wins, and its re-fire is still armed for an instant the
// job no longer waits for; keep nothing from the row where a claim meets the
// hold with nothing kept for the job, and a refusal of the node's gate kept in
// the hold's place leaves nothing kept once its re-fire meets the hold, nor
// anything for the backstop where that claim's row is not read or its hold
// lifted between the claim and the read; keep it for the backstop whatever
// lease owns the row, and a node whose lease the row has passed keeps re-firing
// a job another holder owns; lift a hold on its signal and leave the job to the
// waiter, and a lift with nothing kept for the job, whether dropped or never
// made by a recovery the signal raced, leaves it unannounced; leave a held row
// out of a recovery, and nothing is kept; read what was signalled only once the
// recovery has listed the seat's runs, and a signal during it whose listing of
// the holds the store does not answer leaves the job kept for the hold's
// instant; re-fire the job on a signal's lift while this node keeps it or a
// re-fire of it is on its way, or keep it from the row in a recovery while
// that lift's re-fire is, and it is re-fired twice at once.
func TestACompletionTheBudgetHeldOnItsRowIsKeptForTheHold(t *testing.T) {
	t.Parallel()

	t.Run("re-fired at the hold's instant, never sooner", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		until := rig.now.Add(time.Hour)
		budgetHeldOnRow(t, rig, until)
		if delays := rig.retries.delays(); len(delays) != 1 || delays[0] != time.Hour {
			t.Fatalf("the held completion's re-fires armed = %v; want one, at the hold's instant an hour on",
				delays)
		}
		// THE HOLD'S INSTANT, with nothing signalled and nothing announced.
		rig.now = until
		rig.fireRetries()
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
			t.Fatalf("the turn resumed %d times (%+v) at the hold's instant; want it carried once", len(resumed),
				resumed)
		}
	})

	t.Run("re-fired on its signal once the hold is lifted", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		budgetHeldOnRow(t, rig, rig.now.Add(24*time.Hour))
		// THE SIGNAL'S LISTING OF EVERY HOLD ON ITS CONDITION NOT ANSWERED:
		// the keep's own lift of its row's hold is what releases the job.
		rig.coordinator.pending = unlistedRuns{rig.coordinator.pending}
		rig.coordinator.Readmit("budget", "swe")
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
			t.Fatalf("the turn resumed %d times (%+v) on its condition's signal; want it carried once, the "+
				"hold lifted first", len(resumed), resumed)
		}
	})

	t.Run("kept through a delivery of it the hold refuses", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		until := rig.now.Add(time.Hour)
		budgetHeldOnRow(t, rig, until)
		// ANOTHER DELIVERY OF THE COMPLETION, queued behind the resume that
		// held it: admitted, its claim refused by the hold, and spent with
		// nothing settled.
		store := &countedCalls{PendingStore: rig.coordinator.pending}
		rig.coordinator.pending = store
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(live, payload, ev); err != nil {
			t.Fatalf("the premise: the delivery behind the hold = %v; want it spent", err)
		}
		if resumed := rig.resumer.calls(); len(resumed) != 0 {
			t.Fatalf("the premise: the turn resumed %d times with its hold standing", len(resumed))
		}
		if delays := rig.retries.delays(); len(delays) != 1 || delays[0] != time.Hour {
			t.Fatalf("after a delivery the hold refused the re-fires armed = %v; want the held completion "+
				"still kept, its re-fire at the hold's instant", delays)
		}
		// KEPT STANDING, not dropped and made again from the row: a delivery
		// the hold refuses costs its claim and no read of the row.
		if reads := store.gets.Load(); reads != 0 {
			t.Fatalf("a delivery the hold refused read the run's row %d times; want the keep left standing "+
				"through its admission, and no read", reads)
		}
		rig.now = until
		rig.fireRetries()
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
			t.Fatalf("the turn resumed %d times (%+v) at the hold's instant; want it carried once", len(resumed),
				resumed)
		}
	})

	t.Run("dropped once a claim of it wins", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		budgetHeldOnRow(t, rig, rig.now.Add(time.Hour))
		// THE HOLD LIFTED BY A WRITE THAT LANDED WITH ITS ANSWER LOST: the
		// keep stands, its re-fire armed for the instant still.
		if _, err := rig.pending.LiftHold(live, "t1", "budget"); err != nil {
			t.Fatalf("lifting the hold: %v", err)
		}
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(live, payload, ev); err != nil {
			t.Fatalf("a delivery after the hold lifted = %v; want it carried", err)
		}
		if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
			t.Fatalf("the turn resumed %d times (%+v) after the hold lifted; want it carried once", len(resumed),
				resumed)
		}
		for _, d := range rig.retries.delays() {
			if d == time.Hour {
				t.Fatalf("after a claim of the held completion won, the re-fires armed = %v; want its keep for "+
					"the hold gone with the claim", rig.retries.delays())
			}
		}
	})

	t.Run("kept again where a refusal kept in its place meets the hold", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		until := rig.now.Add(time.Hour)
		budgetHeldOnRow(t, rig, until)
		// A DELIVERY BEHIND THE HOLD THAT THE NODE'S GATE REFUSES: kept for
		// the gate's condition in the hold's place, its re-fire at the
		// backstop.
		gate := gated(rig)
		gate.refuse(true)
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(live, payload, ev); !errors.Is(err, ErrResumeUnavailable) {
			t.Fatalf("the premise: the delivery the gate refused = %v; want it refused", err)
		}
		if delays := rig.retries.delays(); len(delays) != 1 || delays[0] != answerWaitBackstop {
			t.Fatalf("the premise: the re-fires armed = %v; want the gate's refusal kept for the backstop", delays)
		}
		// THE GATE ADMITS AGAIN, and that keep's re-fire meets the hold.
		gate.refuse(false)
		rig.fireRetries()
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 0 {
			t.Fatalf("the premise: the turn resumed %d times with its hold standing", len(resumed))
		}
		if delays := rig.retries.delays(); len(delays) != 1 || delays[0] != time.Hour {
			t.Fatalf("after a re-fire the hold refused the re-fires armed = %v; want the held completion kept "+
				"again, its re-fire at the hold's instant", delays)
		}
		rig.now = until
		rig.fireRetries()
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
			t.Fatalf("the turn resumed %d times (%+v) at the hold's instant; want it carried once", len(resumed),
				resumed)
		}
	})

	t.Run("kept for the backstop where a claim the hold refused reads no row", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		budgetHeldOnRow(t, rig, rig.now.Add(time.Hour))
		gate := gated(rig)
		gate.refuse(true)
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(live, payload, ev); !errors.Is(err, ErrResumeUnavailable) {
			t.Fatalf("the premise: the delivery the gate refused = %v; want it refused", err)
		}
		// THE GATE ADMITS AGAIN, and that keep's re-fire meets the hold while
		// the store does not answer a read of the run's row.
		gate.refuse(false)
		store := &silentStore{PendingStore: rig.coordinator.pending}
		store.silence("Get")
		rig.coordinator.pending = store
		rig.fireRetries()
		rig.coordinator.inflight.Wait()
		if delays := rig.retries.delays(); len(delays) != 1 || delays[0] != answerWaitBackstop {
			t.Fatalf("after a claim the hold refused, its row unread, the re-fires armed = %v; want the "+
				"completion kept for the backstop", delays)
		}
		// THE ROW READ AT THAT BACKSTOP: kept for the hold again.
		store.answer()
		rig.fireRetries()
		rig.coordinator.inflight.Wait()
		if delays := rig.retries.delays(); len(delays) != 1 || delays[0] != time.Hour {
			t.Fatalf("after a claim the hold refused, its row read, the re-fires armed = %v; want the held "+
				"completion kept again, its re-fire at the hold's instant", delays)
		}
	})

	t.Run("kept for the backstop where its hold lifts between a refused claim and the read", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		budgetHeldOnRow(t, rig, rig.now.Add(time.Hour))
		gate := gated(rig)
		gate.refuse(true)
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(live, payload, ev); !errors.Is(err, ErrResumeUnavailable) {
			t.Fatalf("the premise: the delivery the gate refused = %v; want it refused", err)
		}
		// THE GATE ADMITS AGAIN, and that keep's re-fire meets the hold,
		// which its signal lifts before the run's row is read, with nothing
		// kept for the signal to re-fire.
		gate.refuse(false)
		rig.coordinator.pending = liftedOnRefusal{rig.coordinator.pending}
		rig.fireRetries()
		rig.coordinator.inflight.Wait()
		if delays := rig.retries.delays(); len(delays) != 1 || delays[0] != answerWaitBackstop {
			t.Fatalf("after a claim the hold refused, the hold lifted since, the re-fires armed = %v; want the "+
				"completion kept for the backstop", delays)
		}
		rig.fireRetries()
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
			t.Fatalf("the turn resumed %d times (%+v) at the backstop; want it carried once", len(resumed),
				resumed)
		}
	})

	t.Run("nothing kept where the claim met a newer lease's row", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "first pass"})
		// THE SEAT'S NEXT HOLDER HAS TAKEN THE RUN, while this node's lease
		// still reads held: its claim of the completion is refused, and the
		// row, running and unheld, is that holder's to recover.
		if won, err := rig.pending.ClaimOwnership(live, "t1", "node-c:1", rigLease.Epoch+1); err != nil || !won {
			t.Fatalf("the premise: ClaimOwnership = %v, %v", won, err)
		}
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(live, payload, ev); err != nil {
			t.Fatalf("the premise: the delivery under the older lease = %v; want its claim refused", err)
		}
		if resumed := rig.resumer.calls(); len(resumed) != 0 {
			t.Fatalf("the premise: the turn resumed %d times under an older lease", len(resumed))
		}
		if delays := rig.retries.delays(); len(delays) != 0 {
			t.Fatalf("after a claim a newer lease's row refused the re-fires armed = %v; want nothing kept on "+
				"the node whose lease the row has passed", delays)
		}
	})

	t.Run("kept for the instant where the signal's lift is not answered", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		until := rig.now.Add(time.Hour)
		budgetHeldOnRow(t, rig, until)
		counted := &countedCalls{PendingStore: rig.coordinator.pending}
		store := &unliftedHolds{PendingStore: counted}
		store.silent.Store(true)
		rig.coordinator.pending = store
		rig.coordinator.Readmit("budget", "swe")
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 0 {
			t.Fatalf("the premise: the turn resumed %d times with its hold standing", len(resumed))
		}
		if delays := rig.retries.delays(); len(delays) != 1 || delays[0] != time.Hour {
			t.Fatalf("after a lift the store did not answer the re-fires armed = %v; want the held completion "+
				"still kept, its re-fire at the hold's instant", delays)
		}
		// KEPT AS IT WAS, never re-fired into the hold it could not lift.
		if claims := counted.claims.Load(); claims != 0 {
			t.Fatalf("after a lift the store did not answer the job was claimed %d times; want no claim "+
				"before the hold's instant", claims)
		}
		store.silent.Store(false)
		rig.now = until
		rig.fireRetries()
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 {
			t.Fatalf("the turn resumed %d times at the hold's instant; want it carried once", len(resumed))
		}
	})

	t.Run("kept by the seat's recovery after a restart", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		until := rig.now.Add(time.Hour)
		budgetHeldOnRow(t, rig, until)
		// THE NODE RESTARTS, its memory of the keep gone with it.
		rig.restart(t, rig.queue, "", rigLease, nil)
		if err := rig.recoverSeat(t.Context(), rigLease.Owner, rigLease.Epoch); err != nil {
			t.Fatalf("recovering the seat: %v", err)
		}
		rig.coordinator.inflight.Wait()
		// THE NODE BEFORE THE RESTART holds no lease, so only the restarted
		// node's re-fire carries anything.
		var armed []time.Duration
		for _, d := range rig.retries.delays() {
			if d == time.Hour {
				armed = append(armed, d)
			}
		}
		if len(armed) != 2 {
			t.Fatalf("the re-fires armed after the restart = %v; want the restarted node's beside the old "+
				"one's, each at the hold's instant", rig.retries.delays())
		}
		rig.now = until
		rig.fireRetries()
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 {
			t.Fatalf("the turn resumed %d times at the hold's instant after the restart; want it carried once",
				len(resumed))
		}
	})

	t.Run("re-fired on its signal where nothing is kept for it", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		budgetHeldOnRow(t, rig, rig.now.Add(24*time.Hour))
		// NOTHING KEPT HERE FOR THE JOB: the signal's lift of the row's hold
		// is all that releases it.
		rig.coordinator.forgetRefused("t1")
		rig.coordinator.Readmit("budget", "swe")
		rig.coordinator.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
			t.Fatalf("the turn resumed %d times (%+v) once its signal lifted the hold; want it carried once by "+
				"the lift, not left to a waiter that announces nothing", len(resumed), resumed)
		}
	})

	t.Run("re-fired on a signal that lands while the seat's recovery runs", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		budgetHeldOnRow(t, rig, rig.now.Add(24*time.Hour))
		rig.restart(t, rig.queue, "", rigLease, nil)
		// THE CONDITION IS SIGNALLED once the recovery has read the seat's
		// runs: the signal's lift of every hold on the condition lands on
		// the row before the recovery's fence reads it again, or after, and
		// nothing is kept yet for the signal to re-fire either way.
		restarted := rig.coordinator
		restarted.pending = &signalledOnRecovery{PendingStore: restarted.pending,
			signal: func() { restarted.Readmit("budget", "swe") }}
		if err := rig.recoverSeat(live, rigLease.Owner, rigLease.Epoch); err != nil {
			t.Fatalf("recovering the seat: %v", err)
		}
		restarted.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
			t.Fatalf("the turn resumed %d times (%+v) on a signal that landed while the seat's recovery ran; "+
				"want it carried once, not kept for an instant its row no longer holds it to", len(resumed),
				resumed)
		}
	})

	t.Run("re-fired once where the signal's lift lands after the recovery's fence and re-fires it first",
		func(t *testing.T) {
			t.Parallel()
			live := t.Context()
			rig := newCoordRig(t)
			budgetHeldOnRow(t, rig, rig.now.Add(24*time.Hour))
			rig.restart(t, rig.queue, "", rigLease, nil)
			// THE SIGNAL'S LIFT LANDS ONCE THE RECOVERY'S FENCE HAS READ THE
			// ROW HELD, and its re-fire claims the job before the recovery
			// keeps the row it read: the keep finds the signal landed since.
			restarted := rig.coordinator
			counted := &countedCalls{PendingStore: restarted.pending}
			restarted.pending = &signalledOnRecovery{PendingStore: newLiftAfterFence(counted),
				signal: func() { restarted.Readmit("budget", "swe") }}
			recovered := make(chan struct{})
			rig.resumer.beforeBegin = func(context.Context) { <-recovered }
			if err := rig.recoverSeat(live, rigLease.Owner, rigLease.Epoch); err != nil {
				t.Fatalf("recovering the seat: %v", err)
			}
			close(recovered)
			restarted.inflight.Wait()
			if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
				t.Fatalf("the turn resumed %d times (%+v); want it carried once", len(resumed), resumed)
			}
			if claims := counted.claims.Load(); claims != 1 {
				t.Fatalf("the job was claimed %d times; want it re-fired once, by the signal's lift, and "+
					"left as it is by the recovery that found it re-fired", claims)
			}
		})

	t.Run("re-fired once where the signal's lift lands after the recovery's keep has re-fired it",
		func(t *testing.T) {
			t.Parallel()
			live := t.Context()
			rig := newCoordRig(t)
			budgetHeldOnRow(t, rig, rig.now.Add(24*time.Hour))
			rig.restart(t, rig.queue, "", rigLease, nil)
			// THE RECOVERY KEEPS THE ROW IT READ HELD, finds the signal
			// landed since and lifts and re-fires it; the signal's own lift
			// lands before the keep's, and finds the job's re-fire on its way.
			restarted := rig.coordinator
			counted := &countedCalls{PendingStore: restarted.pending}
			recovered := make(chan struct{})
			restarted.pending = &signalledOnRecovery{
				PendingStore: &liftAfterRecovery{PendingStore: counted, recovered: recovered,
					second: make(chan struct{})},
				signal: func() { restarted.Readmit("budget", "swe") }}
			rig.resumer.beforeBegin = func(context.Context) { <-recovered }
			if err := rig.recoverSeat(live, rigLease.Owner, rigLease.Epoch); err != nil {
				t.Fatalf("recovering the seat: %v", err)
			}
			close(recovered)
			restarted.inflight.Wait()
			if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
				t.Fatalf("the turn resumed %d times (%+v); want it carried once", len(resumed), resumed)
			}
			if claims := counted.claims.Load(); claims != 1 {
				t.Fatalf("the job was claimed %d times; want it re-fired once, by the recovery's keep, and "+
					"not again by the signal's lift that found that re-fire on its way", claims)
			}
		})

	t.Run("re-fired on a signal during the seat's recovery whose listing is not answered", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		budgetHeldOnRow(t, rig, rig.now.Add(24*time.Hour))
		rig.restart(t, rig.queue, "", rigLease, nil)
		// THE CONDITION IS SIGNALLED once the recovery has read the seat's
		// runs, and the store does not answer its listing of every hold on
		// the condition: nothing lifts the row's hold but the keep the
		// recovery makes of it.
		restarted := rig.coordinator
		restarted.pending = &signalledOnRecovery{PendingStore: unlistedRuns{restarted.pending},
			signal: func() { restarted.Readmit("budget", "swe") }}
		if err := rig.recoverSeat(live, rigLease.Owner, rigLease.Epoch); err != nil {
			t.Fatalf("recovering the seat: %v", err)
		}
		restarted.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
			t.Fatalf("the turn resumed %d times (%+v) on a signal that landed while the seat's recovery ran, "+
				"its listing of the holds not answered; want it carried once by the recovery's keep, not kept "+
				"for the hold's instant", len(resumed), resumed)
		}
	})

	t.Run("kept where a stop's lift is not answered", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		until := rig.now.Add(time.Hour)
		budgetHeldOnRow(t, rig, until)
		counted := &countedCalls{PendingStore: rig.coordinator.pending}
		store := &unliftedHolds{PendingStore: counted}
		store.silent.Store(true)
		rig.coordinator.pending = store
		if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
			t.Fatalf("StopSeat: %v", err)
		}
		rig.coordinator.inflight.Wait()
		if delays := rig.retries.delays(); len(delays) != 1 || delays[0] != time.Hour {
			t.Fatalf("after a stop whose lift the store did not answer the re-fires armed = %v; want the held "+
				"completion still kept, its re-fire at the hold's instant", delays)
		}
		// KEPT AS IT WAS, never re-fired into the hold it could not lift.
		if claims := counted.claims.Load(); claims != 0 {
			t.Fatalf("after a stop whose lift the store did not answer the job was claimed %d times; want no "+
				"claim before the hold's instant", claims)
		}
	})
}

// A RESUME ITS SEAT'S BUDGET HELD ON ITS ROW IS HELD NO LATER THAN ITS TURN'S
// DEADLINE. Past the deadline nothing waits, and a resume that meets anything
// then ends the run. But nothing reached a held row to end it: the waiter
// passes it over and the claim refuses it, an ending at the deadline's claim
// among them, and the backstop that ends a completion held past its deadline
// is for one this node holds in memory. So a capped turn whose review met the
// budget minutes before its cap held its seat until the budget's window
// turned, a day, a week or a month on. The hold is written until the
// deadline, where it is sooner than the window's turn, and the keep made for
// it re-fires the job at the deadline, whose resume past it ends the run.
//
// Mutation: hold the row until the refusal's instant alone, and the job is
// re-fired, and the run ended, only once the window turns.
func TestABudgetHoldOnTheRowLapsesAtItsTurnsDeadline(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	deadline := rig.now.Add(10 * time.Minute)
	rig.launchUntil("t1", deadline)
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "checked the monitors"})
	rig.resumer.failWith(budgetHeld(rig.now.Add(24 * time.Hour)))
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the premise: the held resume = %v; want its completion spent", err)
	}
	if got := rig.get("t1"); got.Status != StatusRunning || got.HeldFor != "budget" ||
		!got.HeldUntil.Equal(deadline) {
		t.Fatalf("the row is %s held for %q until %v; want running, held for the budget until the turn's "+
			"deadline %v, before the window turns", got.Status, got.HeldFor, got.HeldUntil, deadline)
	}
	if delays := rig.retries.delays(); len(delays) != 1 || delays[0] != 10*time.Minute {
		t.Fatalf("the held completion's re-fires armed = %v; want one, at the turn's deadline", delays)
	}
	// THE DEADLINE, the budget still refusing: the job is re-fired, and its
	// resume past the deadline ends the run.
	rig.now = deadline
	rig.fireRetries()
	rig.coordinator.inflight.Wait()
	if !rig.gone("t1") || !rig.endedWith("t1", types.ExecutorFailureDeadline) {
		t.Fatalf("at its turn's deadline the held run is not ended at its deadline: failures %+v",
			rig.failures())
	}
}

// bubbleRig is a rig whose coordinator reads the clock of the synctest bubble
// it is made in, and arms its timers on it, its run store reading the instant
// the bubble began: a case whose subject is what the coordinator does as that
// clock moves.
func bubbleRig(t *testing.T) *coordRig {
	t.Helper()
	rig := newCoordRigWith(t, func(o *CoordinatorOptions) { o.After, o.Now = nil, time.Now })
	rig.now = time.Now()
	return rig
}

// A HELD COMPLETION PAST ITS TURN'S DEADLINE ENDS WITHIN THE BACKSTOP. A
// completion whose resume met a limit after its claim is held here until the
// limit's instant, and its own re-fire waits for that instant. But past its
// turn's deadline nothing waits: an announcement then ends the run. Where no
// waiter announces the job (these rigs run none, as a host whose store is
// silent announces nothing), nothing asked, and the turn whose deadline passed
// waited for the hold's instant, up to the hour a vendor's window is benched
// for. So the held keep's backstop asks what an announcement asks: before the
// deadline it keeps the hold, resuming nothing, and arms the next backstop;
// past it, the run is ended.
//
// Mutation: arm a held completion's timer for its instant alone, and the run
// is still held past its deadline and a backstop; re-fire it at the backstop
// instead of asking, and it is resumed into the limit; ask without arming the
// next backstop, or without asking for the deadline, and the run is never
// ended.
func TestAHeldCompletionPastItsDeadlineEndsWithinTheBackstop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := bubbleRig(t)
		const deadline = answerWaitBackstop + 2*time.Minute
		gated(rig)
		rig.launchUntil("t1", time.Now().Add(deadline))
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "checked the monitors"})
		rig.resumer.failWith(&TurnRefused{Cause: errors.New("llm chain exhausted: usage limit"),
			Refusal: Refusal{Condition: conditionLimit, Reason: "provider limit: default",
				Until: time.Now().Add(time.Hour)}})
		rig.resumer.failsInTurn = true
		payload, ev := rig.completion("t1")
		var refused *TurnRefused
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); !errors.As(err, &refused) {
			t.Fatalf("the premise: OnCompleted = %v; want the resume's refusal", err)
		}
		if _, held := rig.coordinator.stillRefused(payload); !held {
			t.Fatal("the premise: the completion is not held here")
		}
		// A BACKSTOP ON, SHORT OF THE DEADLINE: still held, nothing resumed into
		// the limit again, the run running.
		time.Sleep(answerWaitBackstop + time.Second)
		synctest.Wait()
		if _, held := rig.coordinator.stillRefused(payload); !held {
			t.Fatal("the backstop before the deadline dropped the hold; want it kept")
		}
		if begun := rig.resumer.turnsBegun(); begun != 1 || rig.gone("t1") {
			t.Fatalf("by the first backstop %d resumed turns began (run gone %v); want only the one that met "+
				"the limit, the run held", begun, rig.gone("t1"))
		}
		// THE DEADLINE PASSES with nothing announcing the job: ended by the next
		// backstop, never resumed.
		// (A backstop and a second slept already: a deadline more is a
		// backstop and a second past it.)
		time.Sleep(deadline)
		synctest.Wait()
		if !rig.gone("t1") || !rig.endedWith("t1", types.ExecutorFailureDeadline) {
			t.Fatalf("a backstop past the deadline the held run is not ended at its deadline: failures %+v",
				rig.failures())
		}
		if begun := rig.resumer.turnsBegun(); begun != 1 {
			t.Fatalf("%d resumed turns began; want only the one that met the limit", begun)
		}
	})
}
