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

// fencedUnderClaim is a run store that orders a seat's recovery pass against
// one claim of a run's job, the first claim a case watches: the pass's first
// fence of a run waits until that claim has settled, by the store's answer to
// it or, refused before the store was asked, by the case's own word
// ([fencedUnderClaim.settle]); and a claim the store let win waits in its turn
// until the pass has read the run's row again after that fence. So a claim the
// node takes inside the pass is a live claim when the pass reads the row, never
// one that has already run its course. It answers every call as the store
// beneath it does.
type fencedUnderClaim struct {
	PendingStore
	settled, reread              chan struct{}
	settleOnce                   sync.Once
	owning, owned, read, claimed atomic.Bool
	claims                       atomic.Int64
}

func newFencedUnderClaim(store PendingStore) *fencedUnderClaim {
	return &fencedUnderClaim{PendingStore: store, settled: make(chan struct{}), reread: make(chan struct{})}
}

// settle says the watched claim settled without reaching the store.
func (s *fencedUnderClaim) settle() { s.settleOnce.Do(func() { close(s.settled) }) }

func (s *fencedUnderClaim) ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error) {
	if s.owning.CompareAndSwap(false, true) {
		select {
		case <-s.settled:
		case <-ctx.Done():
		}
	}
	won, err := s.PendingStore.ClaimOwnership(ctx, turnID, owner, epoch)
	s.owned.Store(true)
	return won, err
}

func (s *fencedUnderClaim) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	run, found, err := s.PendingStore.Get(ctx, turnID)
	if s.owned.Load() && s.read.CompareAndSwap(false, true) {
		close(s.reread)
	}
	return run, found, err
}

func (s *fencedUnderClaim) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	s.claims.Add(1)
	run, won, err := s.PendingStore.ClaimForResume(ctx, turnID, tail, fence)
	if s.claimed.CompareAndSwap(false, true) {
		s.settle()
		if err == nil && won {
			select {
			case <-s.reread:
			case <-ctx.Done():
			}
		}
	}
	return run, won, err
}

// settlingGate is a [TurnGate] that admits every resume and says, on the
// release of the first admission it grants, that the watched claim settled
// ([fencedUnderClaim.settle]): a claim refused before the store returns through
// that release. It counts what it is asked.
type settlingGate struct {
	store *fencedUnderClaim
	asked atomic.Int64
}

func (g *settlingGate) admit(ctx context.Context, _ string, _ ResumeRoute) (context.Context, func(), error) {
	g.asked.Add(1)
	return ctx, g.store.settle, nil
}

// abandoned reports whether any run of the rig was announced ended as a tail
// its node abandoned.
func (r *coordRig) abandoned() bool {
	for _, failed := range r.failures() {
		if failed.Reason == types.ExecutorFailureAbandoned {
			return true
		}
	}
	return false
}

// A SEAT'S RECOVERY PASS AND THE CLAIMS OF ITS RUNS ON THIS NODE EXCLUDE EACH
// OTHER. The pass reads a claim it finds as one whose node stopped, and reaps
// it as abandoned, or revives its answer. But an apply that brings the code-work
// runtime up on a seat already held runs that pass with the seat's admission
// open, so a signal's re-fire, a delivery or an owed answer's attempt claimed a
// run while the pass ran, and a completion past its deadline is claimed with no
// admission on any road, the acquisition's included: the pass then fenced this
// node's own live claim, read it again as a resumed tail and reaped it, ending
// the turn under the review still running it (or reviving the answer the
// attempt was resuming with). Every claim passes one gate that refuses it while
// the pass runs, and what it refuses is carried: a re-fire, a held completion's
// backstop and an owed answer by the pass when it is done, and a delivery by the
// queue, handed back as the node's failure. A claim the gate let through before
// the pass began is the attempt's, which the reap refuses to touch: for as long
// as that attempt is out, and for its own run alone, so a claim whose attempt
// has returned, and a dead claim beside a live one, are still tails the pass
// reaps.
//
// Each case orders the claim inside the pass with a store that holds the pass's
// fence until the claim settled and a claim that won until the pass has read
// the row again ([fencedUnderClaim]), never by the odds of a race.
//
// Mutation: drop the gate from the completion's claim, and a re-fire, a
// delivery and a deadline's claim each take the run inside the pass, which reaps
// it; from the answer's claim, and the pass revives the answer the attempt is
// resuming; open the gate before the control topic is attached, or never close
// it, and a claim lands inside the pass; keep a refused re-fire for its
// backstop, and the job waits five minutes past the pass; leave a refused
// answer or a held completion's backstop to nothing, and neither is carried;
// hand a refused delivery back as the message's, and it counts toward its
// poison; let the reap end a claim this node's own attempt holds, and a claim
// let through before the pass is reaped under its turn; never count an attempt
// off, and the claim of one that returned is reaped by no later pass; count
// attempts by the seat rather than the run, and a dead claim beside a live one
// is never reaped.
func TestASeatsRecoveryAndTheClaimsOfItsRunsExcludeEachOther(t *testing.T) {
	t.Parallel()

	t.Run("a signal's re-fire whose lift lands before the fence is carried past the pass", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		budgetHeldOnRow(t, rig, rig.now.Add(24*time.Hour))
		// THE SEAT ALREADY HELD, the runtime brought up by an apply: a fresh
		// coordinator whose admission admits, recovering under the lease it
		// holds. The signal lands once the pass has read the seat's runs, its
		// lift lands before the fence, and its re-fire is admitted and claims.
		store := newFencedUnderClaim(rig.coordinator.pending)
		gate := &settlingGate{store: store}
		rig.restart(t, rig.queue, "", rigLease, gate.admit)
		restarted := rig.coordinator
		restarted.pending = &signalledOnRecovery{PendingStore: store,
			signal: func() { restarted.Readmit("budget", "swe") }}
		if err := rig.recoverSeat(live, rigLease.Owner, rigLease.Epoch); err != nil {
			t.Fatalf("recovering the seat: %v", err)
		}
		if rig.abandoned() {
			t.Fatalf("the seat's recovery reaped the run a re-fire of this node's claimed while it ran: "+
				"failures %+v", rig.failures())
		}
		restarted.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
			t.Fatalf("the turn resumed %d times (%+v); want it carried once, past the pass", len(resumed), resumed)
		}
		if claims := store.claims.Load(); claims != 1 {
			t.Fatalf("the store was asked %d claims of the job; want one, the re-fire the pass carried, none "+
				"inside the pass", claims)
		}
		if asked := gate.asked.Load(); asked != 2 {
			t.Fatalf("the admission was asked %d times; want twice: the re-fire the recovery gate refused, "+
				"and the one the pass made when it was done", asked)
		}
		if rig.abandoned() {
			t.Fatalf("the run was reaped: failures %+v", rig.failures())
		}
	})

	t.Run("a delivery inside the pass on a seat already held is handed back", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
		_, ev := rig.completion("t1")
		store := newFencedUnderClaim(rig.coordinator.pending)
		rig.restart(t, rig.queue, "", rigLease, nil)
		restarted := rig.coordinator
		// THE CONTROL TOPIC DELIVERS once the pass has read the seat's runs.
		var outcome error
		delivered := make(chan struct{})
		restarted.pending = &signalledOnRecovery{PendingStore: store, signal: func() {
			go func() {
				defer close(delivered)
				defer store.settle()
				outcome = restarted.OnEvent(delivery(live, ev), ev)
			}()
		}}
		if err := rig.recoverSeat(live, rigLease.Owner, rigLease.Epoch); err != nil {
			t.Fatalf("recovering the seat: %v", err)
		}
		<-delivered
		if rig.abandoned() {
			t.Fatalf("the seat's recovery reaped the run a delivery claimed while it ran: failures %+v",
				rig.failures())
		}
		if !errors.Is(outcome, ErrSeatRecovering) {
			t.Fatalf("the delivery inside the pass = %v; want it refused by the seat's recovery gate", outcome)
		}
		if got := controlResult(outcome); got.Outcome != queue.OutcomeNak || got.FailsMessage() {
			t.Fatalf("the refused delivery = %s (fails the message %v); want it handed back as the node's",
				got.Outcome, got.FailsMessage())
		}
		if begun := rig.resumer.turnsBegun(); begun != 0 {
			t.Fatalf("%d turns began inside the pass; want none", begun)
		}
		// THE BROKER BRINGS IT BACK past the pass: carried once.
		if err := restarted.OnEvent(delivery(live, ev), ev); err != nil {
			t.Fatalf("the delivery handed back, delivered again: %v", err)
		}
		restarted.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
			t.Fatalf("the turn resumed %d times (%+v); want it carried once", len(resumed), resumed)
		}
	})

	t.Run("a delivery made as the control topic is attached waits for the pass", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
		_, ev := rig.completion("t1")
		rig.restart(t, rig.queue, "", rigLease, nil)
		restarted := rig.coordinator
		// THE SUBSCRIPTION DELIVERS the moment it is attached, before the
		// pass has read a run.
		var outcome error
		attach := func(ctx context.Context) error {
			outcome = restarted.OnEvent(delivery(ctx, ev), ev)
			return nil
		}
		if err := restarted.RecoverSeat(live, "swe", rigLease.Owner, rigLease.Epoch, rigInbox, attach); err != nil {
			t.Fatalf("recovering the seat: %v", err)
		}
		if !errors.Is(outcome, ErrSeatRecovering) {
			t.Fatalf("the delivery made as the control topic was attached = %v; want it refused by the seat's "+
				"recovery gate, which closes before the topic is attached", outcome)
		}
		if err := restarted.OnEvent(delivery(live, ev), ev); err != nil {
			t.Fatalf("the delivery handed back, delivered again: %v", err)
		}
		restarted.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
			t.Fatalf("the turn resumed %d times (%+v); want it carried once", len(resumed), resumed)
		}
	})

	t.Run("a completion past its deadline claimed without admission is handed back", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		deadline := rig.now.Add(time.Minute)
		rig.launchUntil("t1", deadline)
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "checked the monitors"})
		_, ev := rig.completion("t1")
		rig.now = deadline.Add(time.Minute)
		// THE SEAT'S ACQUISITION: its admission refuses every turn while it
		// establishes, so the completion, past its turn's deadline, is
		// claimed to be ended with no admission at all.
		store := newFencedUnderClaim(rig.coordinator.pending)
		gate := &turnGate{refusing: true}
		rig.restart(t, rig.queue, "", rigLease, gate.admit)
		restarted := rig.coordinator
		var outcome error
		delivered := make(chan struct{})
		restarted.pending = &signalledOnRecovery{PendingStore: store, signal: func() {
			go func() {
				defer close(delivered)
				defer store.settle()
				outcome = restarted.OnEvent(delivery(live, ev), ev)
			}()
		}}
		if err := rig.recoverSeat(live, rigLease.Owner, rigLease.Epoch); err != nil {
			t.Fatalf("recovering the seat: %v", err)
		}
		<-delivered
		if rig.abandoned() {
			t.Fatalf("the seat's recovery reaped the run its deadline's claim took while it ran: failures %+v",
				rig.failures())
		}
		if !errors.Is(outcome, ErrSeatRecovering) {
			t.Fatalf("the deadline's claim inside the pass = %v; want it refused by the seat's recovery gate",
				outcome)
		}
		if got := controlResult(outcome); got.Outcome != queue.OutcomeNak || got.FailsMessage() {
			t.Fatalf("the refused delivery = %s (fails the message %v); want it handed back as the node's",
				got.Outcome, got.FailsMessage())
		}
		// THE BROKER BRINGS IT BACK past the pass: ended at its deadline, once.
		if err := restarted.OnEvent(delivery(live, ev), ev); err != nil {
			t.Fatalf("the delivery handed back, delivered again: %v", err)
		}
		restarted.inflight.Wait()
		if !rig.gone("t1") || !rig.endedWith("t1", types.ExecutorFailureDeadline) || rig.abandoned() {
			t.Fatalf("the run is not ended at its deadline once: gone %v, failures %+v", rig.gone("t1"),
				rig.failures())
		}
		if begun := rig.resumer.turnsBegun(); begun != 0 {
			t.Fatalf("%d turns began; want none past the deadline", begun)
		}
	})

	t.Run("an owed answer's claim inside the pass waits for it", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		parksOnAQuestion(t, rig, "t1")
		store := newFencedUnderClaim(rig.coordinator.pending)
		rig.restart(t, rig.queue, "", rigLease, nil)
		restarted := rig.coordinator
		// THE PERSON'S REPLY reaches the seat once the pass has read its
		// runs, and is taken as the answer at once.
		done := make(chan struct{})
		restarted.pending = &signalledOnRecovery{PendingStore: store, signal: func() {
			go func() {
				defer close(done)
				defer store.settle()
				_, _ = restarted.TryResumeFromAnswer(live, "swe", chatReply(answerOnTheDM, "use main", nil))
			}()
		}}
		if err := rig.recoverSeat(live, rigLease.Owner, rigLease.Epoch); err != nil {
			t.Fatalf("recovering the seat: %v", err)
		}
		<-done
		if got := rig.get("t1"); got.Answer == nil || got.Answer.LostClaims != 0 || rig.abandoned() {
			t.Fatalf("the seat's recovery took the answer's claim this node made while it ran for a dead one "+
				"(answer %+v, failures %+v); want the claim refused, the answer owed", got.Answer, rig.failures())
		}
		rig.fireRetries()
		restarted.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 {
			t.Fatalf("the turn resumed %d times with the answer; want once, past the pass", len(resumed))
		}
	})

	t.Run("an owed answer recorded once the pass read its run is armed when the pass ends", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		parksOnAQuestion(t, rig, "t1")
		rig.restart(t, rig.queue, "", rigLease, nil)
		restarted := rig.coordinator
		// THE REPLY LANDS ONCE THE PASS HAS FENCED THE RUN AND READ IT
		// PARKED, so the pass counts it parked and queues nothing for it:
		// only what the gate hands the pass carries the answer.
		store := &answeredOnReread{PendingStore: restarted.pending, answer: func() {
			_, _ = restarted.TryResumeFromAnswer(live, "swe", chatReply(answerOnTheDM, "use main", nil))
		}}
		restarted.pending = store
		if err := rig.recoverSeat(live, rigLease.Owner, rigLease.Epoch); err != nil {
			t.Fatalf("recovering the seat: %v", err)
		}
		if begun := rig.resumer.turnsBegun(); begun != 0 {
			t.Fatalf("%d turns began inside the pass; want none", begun)
		}
		if got := rig.get("t1"); got.Status != StatusAnswered {
			t.Fatalf("the premise: the run is %q after the pass; want it answered and owed", got.Status)
		}
		if fired := rig.fireRetries(); fired == 0 {
			t.Fatal("nothing was armed for the answer the recovery gate refused; want its next attempt armed " +
				"when the pass ended")
		}
		restarted.inflight.Wait()
		if resumed := rig.resumer.calls(); len(resumed) != 1 {
			t.Fatalf("the turn resumed %d times with the answer; want once, past the pass", len(resumed))
		}
	})

	t.Run("a held completion's backstop past its deadline is re-fired when the pass ends", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			rig := bubbleRig(t)
			gated(rig)
			rig.launchUntil("t1", time.Now().Add(time.Minute))
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
			// A PASS OF THE SEAT ON THIS NODE, during which the held
			// completion's backstop comes round, the turn's deadline past:
			// its claim to end the run is made inside the pass.
			store := newFencedUnderClaim(rig.coordinator.pending)
			rig.coordinator.pending = &signalledOnRecovery{PendingStore: store, signal: func() {
				time.Sleep(answerWaitBackstop + time.Second)
				store.settle()
			}}
			if err := rig.recoverSeat(t.Context(), rigLease.Owner, rigLease.Epoch); err != nil {
				t.Fatalf("recovering the seat: %v", err)
			}
			synctest.Wait()
			if rig.abandoned() {
				t.Fatalf("the seat's recovery reaped the run its held completion's backstop claimed while it "+
					"ran: failures %+v", rig.failures())
			}
			if !rig.gone("t1") || !rig.endedWith("t1", types.ExecutorFailureDeadline) {
				t.Fatalf("once the pass was done the held run is not ended at its deadline: failures %+v",
					rig.failures())
			}
			if begun := rig.resumer.turnsBegun(); begun != 1 {
				t.Fatalf("%d resumed turns began; want only the one that met the limit", begun)
			}
		})
	})

	t.Run("a claim this node's attempt took before the pass is left to it", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
		// A DELIVERY CLAIMS THE JOB AND HOLDS ITS TURN before it begins, and
		// a pass of the seat runs on this node meanwhile.
		begin, release := make(chan struct{}), make(chan struct{})
		var beginOnce sync.Once
		rig.resumer.beforeBegin = func(context.Context) {
			beginOnce.Do(func() { close(begin) })
			<-release
		}
		payload, ev := rig.completion("t1")
		resumed := make(chan error, 1)
		go func() { resumed <- rig.coordinator.OnCompleted(live, payload, ev) }()
		<-begin
		if err := rig.recoverSeat(live, rigLease.Owner, rigLease.Epoch); err != nil {
			t.Fatalf("recovering the seat: %v", err)
		}
		if got := rig.get("t1"); got.Status != StatusResumed || rig.abandoned() {
			t.Fatalf("the pass reaped the claim this node's live attempt held (run %q, failures %+v); want it "+
				"left to that attempt", got.Status, rig.failures())
		}
		if runs, _ := rig.coordinator.SeatRuns("swe"); !runs {
			t.Fatal("the pass counted the live claim in no set; want it holding the seat")
		}
		close(release)
		if err := <-resumed; err != nil {
			t.Fatalf("the attempt the pass left alone: %v", err)
		}
		rig.coordinator.inflight.Wait()
		if calls := rig.resumer.calls(); len(calls) != 1 || !calls[0].Success {
			t.Fatalf("the turn resumed %d times (%+v); want it carried once, by the attempt", len(calls), calls)
		}
	})

	t.Run("a claim whose attempt has returned is reaped by the seat's next pass", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
		// THE CLAIM LANDS AND THE STORE'S ANSWER IS LOST: the attempt returns
		// with the run claimed and nothing of this node's working on it.
		rig.coordinator.pending = &claimLandsUnanswered{PendingStore: rig.coordinator.pending}
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(live, payload, ev); !errors.Is(err, ErrStoreSilent) {
			t.Fatalf("the premise: the claim whose answer was lost = %v; want the store's silence", err)
		}
		if got := rig.get("t1"); got.Status != StatusResumed {
			t.Fatalf("the premise: the run is %q; want the claim that landed", got.Status)
		}
		// THE SEAT LET GO OF AND TAKEN AGAIN by this node, under a newer lease.
		rig.coordinator.ReleaseSeat("swe")
		if err := rig.recoverSeatOn(live, rigLease.Owner, rigLease.Epoch+1, rigInbox); err != nil {
			t.Fatalf("recovering the seat: %v", err)
		}
		if !rig.gone("t1") || !rig.endedWith("t1", types.ExecutorFailureAbandoned) {
			t.Fatalf("the pass left a claim whose attempt had returned (failures %+v); want it reaped as the "+
				"abandoned tail it is", rig.failures())
		}
	})

	t.Run("a dead claim beside this node's live one is reaped", func(t *testing.T) {
		t.Parallel()
		live := t.Context()
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.launch("t2")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
		// t2's CLAIM IS ONE NO ATTEMPT HOLDS: taken, and its attempt gone.
		if _, won, err := rig.pending.ClaimForResume(live, "t2", CompletionTail(rig.get("t2").LaunchID),
			rigLease); err != nil || !won {
			t.Fatalf("the premise: claiming t2 = %v, %v", won, err)
		}
		// t1's IS A DELIVERY'S, which holds its turn before it begins while a
		// pass of the seat runs on this node.
		begin, release := make(chan struct{}), make(chan struct{})
		var beginOnce sync.Once
		rig.resumer.beforeBegin = func(context.Context) {
			beginOnce.Do(func() { close(begin) })
			<-release
		}
		payload, ev := rig.completion("t1")
		resumed := make(chan error, 1)
		go func() { resumed <- rig.coordinator.OnCompleted(live, payload, ev) }()
		<-begin
		if err := rig.recoverSeat(live, rigLease.Owner, rigLease.Epoch); err != nil {
			t.Fatalf("recovering the seat: %v", err)
		}
		if !rig.gone("t2") || !rig.endedWith("t2", types.ExecutorFailureAbandoned) {
			t.Fatalf("the pass left t2's dead claim beside t1's live one (failures %+v); want it reaped",
				rig.failures())
		}
		if got := rig.get("t1"); got.Status != StatusResumed || rig.endedWith("t1", types.ExecutorFailureAbandoned) {
			t.Fatalf("the pass reaped t1's live claim (run %q, failures %+v); want it left to its attempt",
				got.Status, rig.failures())
		}
		close(release)
		if err := <-resumed; err != nil {
			t.Fatalf("the attempt the pass left alone: %v", err)
		}
		rig.coordinator.inflight.Wait()
		if calls := rig.resumer.calls(); len(calls) != 1 || !calls[0].Success {
			t.Fatalf("t1's turn resumed %d times (%+v); want it carried once, by its attempt", len(calls), calls)
		}
	})
}

// claimLandsUnanswered is a run store that lets the first claim of a job it is
// asked land and then answers it as a store that did not answer: the claim is
// on the row, and the attempt that made it holds nothing of it. It answers
// every other call as the store beneath it does.
type claimLandsUnanswered struct {
	PendingStore
	lost atomic.Bool
}

func (s *claimLandsUnanswered) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	run, won, err := s.PendingStore.ClaimForResume(ctx, turnID, tail, fence)
	if err != nil || !won || !s.lost.CompareAndSwap(false, true) {
		return run, won, err
	}
	return PendingRun{}, false, fmt.Errorf("sandbox: claim run %s: %w", turnID, errStoreUnanswered)
}

// answeredOnReread is a run store that records a person's answer to a run once
// a recovery pass has fenced the run and read it again, before that read
// returns: answer runs then, and the read answers the row as it was before it.
// It answers every call as the store beneath it does.
type answeredOnReread struct {
	PendingStore
	answer      func()
	owned, read atomic.Bool
}

func (s *answeredOnReread) ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error) {
	won, err := s.PendingStore.ClaimOwnership(ctx, turnID, owner, epoch)
	s.owned.Store(true)
	return won, err
}

func (s *answeredOnReread) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	run, found, err := s.PendingStore.Get(ctx, turnID)
	if s.owned.Load() && s.read.CompareAndSwap(false, true) {
		s.answer()
	}
	return run, found, err
}
