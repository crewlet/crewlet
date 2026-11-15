package sandbox

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

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
// Mutation: arm a held completion's re-fire no later than the backstop, as
// the gate's own refusals are, and the job is resumed again five minutes on,
// into the same limit.
func TestACompletionHeldHereIsReFiredAtItsInstantAndNeverSooner(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := newCoordRigWith(t, func(o *CoordinatorOptions) { o.After = nil })
		const lifts = 4 * answerWaitBackstop
		limitedAfterClaim(t, rig, rig.now.Add(lifts))
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
