package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
)

// conditionGate is a [TurnGate] a case moves by hand: while refusing, it
// refuses every resume on cond, and it admits every other; it counts every
// resume it is asked about.
type conditionGate struct {
	mu       sync.Mutex
	cond     Condition
	refusing bool
	asked    int
}

func (g *conditionGate) admit(ctx context.Context, _ string, _ ResumeRoute) (context.Context, func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked++
	if g.refusing {
		return nil, nil, &TurnRefused{Refusal: Refusal{Condition: g.cond,
			Reason: "the seat may not start the turn yet: " + string(g.cond)}}
	}
	return ctx, func() {}, nil
}

// refuse has the gate refuse on cond from here on, or admit where cond is
// empty.
func (g *conditionGate) refuse(cond Condition) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cond, g.refusing = cond, cond != ""
}

func (g *conditionGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.asked
}

// AN ACKNOWLEDGED REFUSAL IS AN OBLIGATION THE SEAT'S HOLDER OWES, AND IT
// OUTLIVES THE HOLDER. A completion the node's gate refuses on a condition that
// clears by waiting is acknowledged and kept for the condition's signal; but
// while the coordination store does not answer the host of the job's box, that
// host's waiter reads the run's row through it and announces nothing, and a
// silence of the store's runs alone has no bound. Kept in the holder's memory
// alone, the completion went with a restart, or with the seat moving to its
// next holder, before the signal came, and nothing announced the job again for
// as long as that silence lasted. So every refusal the holder acknowledges is
// recorded on the job's record, and the seat's recovery, a restart's and the
// next holder's alike, re-arms it and asks its own gate at once, as the first
// delivery would: a refusal there is kept for that holder's own condition,
// whose signal carries the completion.
//
// Mutation: acknowledge the refusal without recording it, or recover the seat
// without reading the record, and the completion is never carried; re-arm the
// record on its recorded condition alone, and the next holder, whose own gate
// refuses on another, never re-fires it.
func TestAnAcknowledgedRefusalOutlivesItsHolder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// node and fence are who recovers the seat, under which lease: this
		// node again after a restart, or the seat's next holder, under a
		// newer one.
		node  string
		fence Fence
		// own is the condition the holder that recovers the seat refuses on
		// until it is signalled: the one the refusal it recovers named, or
		// one of its own.
		own Condition
	}{
		{name: "after a restart, on its condition's signal", node: "node-b", fence: rigLease,
			own: conditionLimit},
		{name: "on the seat's next holder, on its condition's signal", node: "node-c",
			fence: Fence{Owner: "node-c:1", Epoch: rigLease.Epoch + 1}, own: conditionLimit},
		{name: "on the seat's next holder, on a condition of its own", node: "node-c",
			fence: Fence{Owner: "node-c:1", Epoch: rigLease.Epoch + 1}, own: Draining},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			// NODE-B HOLDS THE SEAT, and the job's box is on node-a's host,
			// whose coordination store does not answer its read of the run's
			// row: its waiter announces nothing, and no collection has found
			// that yet.
			host, _ := unsettledHost(t, rig)
			// THE GATE REFUSES THE COMPLETION on a condition that clears by
			// waiting, and its delivery is acknowledged.
			gate := &conditionGate{}
			gate.refuse(conditionLimit)
			rig.coordinator.gate = gate.admit
			_, ev := rig.completion("t1")
			if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); controlResult(err).Outcome != queue.OutcomeAck {
				t.Fatalf("the premise: the refused delivery = %v, left %s; want it acknowledged", err,
					controlResult(err).Outcome)
			}
			// THE HOLDER RESTARTS, or the seat moves: nothing of its memory
			// is left. The holder that recovers the seat refuses until the
			// condition it refuses on is signalled.
			next := &conditionGate{}
			next.refuse(tc.own)
			rig.restart(t, rig.queue, tc.node, tc.fence, next.admit)
			if err := rig.recoverSeat(t.Context(), tc.fence.Owner, tc.fence.Epoch); err != nil {
				t.Fatalf("recovering the seat: %v", err)
			}
			rig.coordinator.inflight.Wait()
			if resumed := rig.resumer.calls(); len(resumed) != 0 {
				t.Fatalf("the turn resumed %d times while the recovering holder's gate refused it", len(resumed))
			}
			// THE HOST'S STORE ANSWERS AGAIN, and its waiter has announced
			// nothing; the condition clears and is signalled.
			host.answer()
			next.refuse("")
			rig.coordinator.Readmit(tc.own, "swe")
			rig.coordinator.inflight.Wait()
			if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
				t.Fatalf("the turn resumed %d times (%+v) once its condition was signalled; want it carried "+
					"once, on its box's account", len(resumed), resumed)
			}
		})
	}
}

// claimedFirst is a run store on which another attempt's claim of a job's
// completion lands before each owed refusal of it is recorded
// ([PendingStore.OweRefusal]), under fence: the claim of a re-fire this node
// started for the kept completion the moment its condition was signalled,
// between the gate's refusal of a delivery and that refusal's record.
type claimedFirst struct {
	PendingStore
	fence Fence
}

func (s *claimedFirst) OweRefusal(ctx context.Context, turnID, launch string, owed OwedRefusal,
	fence Fence,
) (bool, error) {
	_, won, err := s.ClaimForResume(ctx, turnID, CompletionTail(launch), s.fence)
	switch {
	case err != nil:
		return false, fmt.Errorf("the premise: another attempt's claim of %s: %w", turnID, err)
	case !won:
		return false, fmt.Errorf("the premise: another attempt's claim of %s did not win", turnID)
	}
	return s.PendingStore.OweRefusal(ctx, turnID, launch, owed, fence)
}

// A REFUSAL ACKNOWLEDGED WHILE ANOTHER ATTEMPT HOLDS THE JOB'S CLAIM IS OWED
// TOO. A kept completion is re-fired on a goroutine of its own the moment its
// condition is signalled, so a re-fire's claim can land between the gate's
// refusal of a delivery and that refusal's record. A claim settles nothing by
// itself: one handed back, its attempt cut short by this node's stop before
// its collection heard from the box's host, puts the completion back as it
// was. Recorded only on a job running and unclaimed, the refusal acknowledged
// meanwhile was owed by nothing once that claim was handed back, and after the
// restart no recovery re-fired the job, which no waiter announces while its
// host's store is silent.
//
// Mutation: record an owed refusal only where the job is running and
// unclaimed, and the job's record owes nothing once the claim is handed back,
// and the completion is never carried.
func TestARefusalAcknowledgedUnderAnotherAttemptsClaimIsOwed(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	// NODE-B HOLDS THE SEAT, and the job's box is on node-a's host, whose
	// coordination store does not answer its read of the run's row.
	host, _ := unsettledHost(t, rig)
	plain := rig.coordinator.pending
	rig.coordinator.pending = &claimedFirst{PendingStore: plain, fence: rigLease}
	gate := &conditionGate{}
	gate.refuse(conditionLimit)
	rig.coordinator.gate = gate.admit
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); controlResult(err).Outcome != queue.OutcomeAck {
		t.Fatalf("the premise: the refused delivery = %v, left %s; want it acknowledged", err,
			controlResult(err).Outcome)
	}
	// THE OTHER ATTEMPT IS CUT SHORT by this node's stop before its
	// collection hears from the box's host, and its claim is handed back
	// uncounted.
	released, err := plain.ReleaseClaim(t.Context(), "t1",
		Release{Launch: payload.LaunchID, To: StatusRunning, Fence: rigLease})
	if err != nil || !released {
		t.Fatalf("the premise: handing the other attempt's claim back = %v, %v", released, err)
	}
	if got := rig.get("t1"); got.Status != StatusRunning || got.Launch.RefusalOwed != conditionLimit {
		t.Fatalf("the claim handed back left the run %s, its record owing %q; want it running and owing the "+
			"refusal acknowledged under the claim, %q", got.Status, got.Launch.RefusalOwed, conditionLimit)
	}
	// THE NODE RESTARTS, the host's store still silent; its recovery owes the
	// completion again, and carries it once the condition is signalled.
	rig.coordinator.pending = plain
	next := &conditionGate{}
	next.refuse(conditionLimit)
	rig.restart(t, rig.queue, "node-b", rigLease, next.admit)
	if err := rig.recoverSeat(t.Context(), rigLease.Owner, rigLease.Epoch); err != nil {
		t.Fatalf("recovering the seat: %v", err)
	}
	rig.coordinator.inflight.Wait()
	host.answer()
	next.refuse("")
	rig.coordinator.Readmit(conditionLimit, "swe")
	rig.coordinator.inflight.Wait()
	if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
		t.Fatalf("the turn resumed %d times (%+v) once its condition was signalled; want it carried once, on "+
			"its box's account", len(resumed), resumed)
	}
}

// owedRefusals is a run store that counts the owed refusals it records
// ([PendingStore.OweRefusal]) and does not answer that write while silent is
// set, as a coordination store that did not answer it does.
type owedRefusals struct {
	PendingStore
	silent  atomic.Bool
	written atomic.Int64
}

func (s *owedRefusals) OweRefusal(ctx context.Context, turnID, launch string, owed OwedRefusal,
	fence Fence,
) (bool, error) {
	if s.silent.Load() {
		return false, fmt.Errorf("sandbox: recording run %s's owed refusal: %w", turnID, errStoreUnanswered)
	}
	wrote, err := s.PendingStore.OweRefusal(ctx, turnID, launch, owed, fence)
	if wrote {
		s.written.Add(1)
	}
	return wrote, err
}

// EVERY REFUSAL THE CONTROL TOPIC ACKNOWLEDGES IS RECORDED ON THE JOB'S RECORD
// BEFORE IT IS, BY ONE WRITE, AND ONLY ONCE. The gate's refusal and a
// completion this node holds off itself after its resume met a limit alike,
// wherever the box is hosted: the record names the condition and when it was
// acknowledged, a re-announcement refused again writes nothing and leaves the
// first standing, and a record the store does not take is no acknowledgement:
// the delivery is handed back as this node's failure, to be recorded when it
// comes back; so is one this node's own ended context cut, which is handed back
// as the node's, never with the refusal that would have it acknowledged. A row
// a newer lease owns refuses the record, and the delivery is not acknowledged
// by the node that lost the seat. Nor is it by a node whose lease went before
// the record: nothing is written, and the refusal goes on to the seat's holder
// as a resume another node can make (ErrResumeElsewhere), which the engine
// hands back as the node's while its lease reads held again.
//
// Mutation: acknowledge a refusal without recording it, and its record is
// missing and a record the store did not take is acknowledged; carry the
// refusal behind a record its context cut, and that refusal is acknowledged
// unrecorded; record it under a lease gone, or route it on as one no node
// gets past, and that row goes red; write it on every refusal, and the first
// refusal's instant is lost.
func TestAnAcknowledgedRefusalIsRecordedOnItsJob(t *testing.T) {
	t.Parallel()
	// owing builds a rig whose node-b refuses t1's completion on
	// conditionLimit, its box on node-a's host, and counts the owed
	// refusals its store records.
	owing := func(t *testing.T) (*coordRig, *owedRefusals, *conditionGate) {
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		store := &owedRefusals{PendingStore: rig.coordinator.pending}
		rig.coordinator.pending = store
		gate := &conditionGate{}
		gate.refuse(conditionLimit)
		rig.coordinator.gate = gate.admit
		return rig, store, gate
	}

	t.Run("the gate's refusal, recorded once", func(t *testing.T) {
		t.Parallel()
		rig, store, _ := owing(t)
		at := rig.now
		_, ev := rig.completion("t1")
		if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); controlResult(err).Outcome != queue.OutcomeAck {
			t.Fatalf("the refused delivery = %v; want it acknowledged", err)
		}
		got := rig.get("t1")
		if got.Launch.RefusalOwed != conditionLimit || !got.Launch.RefusalOwedSince.Equal(at) {
			t.Fatalf("the job's record owes %q since %v; want %q since %v", got.Launch.RefusalOwed,
				got.Launch.RefusalOwedSince, conditionLimit, at)
		}
		// REFUSED AGAIN, a minute on: nothing more is written, and the first
		// refusal's instant stands.
		rig.now = rig.now.Add(time.Minute)
		_, again := rig.completion("t1")
		err := rig.coordinator.OnEvent(delivery(t.Context(), again), again)
		if controlResult(err).Outcome != queue.OutcomeAck {
			t.Fatalf("the re-announcement refused again = %v; want it acknowledged", err)
		}
		if got := rig.get("t1"); store.written.Load() != 1 || !got.Launch.RefusalOwedSince.Equal(at) {
			t.Fatalf("%d records written, owed since %v; want the one, since %v", store.written.Load(),
				got.Launch.RefusalOwedSince, at)
		}
	})

	t.Run("a completion this node holds off itself", func(t *testing.T) {
		t.Parallel()
		rig, store, _ := owing(t)
		payload, ev := rig.completion("t1")
		rig.coordinator.keepRefused(payload, ev, Refusal{Condition: conditionLimit,
			Reason: "the review's models are at their limit", Until: rig.now.Add(time.Hour)}, true,
			rig.coordinator.signalsSeen())
		err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev)
		if controlResult(err).Outcome != queue.OutcomeAck {
			t.Fatalf("the held completion's delivery = %v; want it acknowledged", err)
		}
		if got := rig.get("t1"); got.Launch.RefusalOwed != conditionLimit || store.written.Load() != 1 {
			t.Fatalf("the job's record owes %q after %d writes; want the hold recorded once",
				got.Launch.RefusalOwed, store.written.Load())
		}
	})

	t.Run("a record the store does not take is no acknowledgement", func(t *testing.T) {
		t.Parallel()
		rig, store, _ := owing(t)
		store.silent.Store(true)
		_, ev := rig.completion("t1")
		err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev)
		if got := controlResult(err); !errors.Is(err, ErrStoreSilent) || got.Outcome != queue.OutcomeNak {
			t.Fatalf("the refusal whose record the store did not take = %v, left %s; want it handed back as "+
				"the store's silence", err, got.Outcome)
		}
		// AND RECORDED WHEN IT COMES BACK, the store answering.
		store.silent.Store(false)
		if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); controlResult(err).Outcome != queue.OutcomeAck {
			t.Fatalf("the delivery back = %v; want it acknowledged once recorded", err)
		}
		if got := rig.get("t1"); got.Launch.RefusalOwed != conditionLimit {
			t.Fatalf("the job's record owes %q; want %q", got.Launch.RefusalOwed, conditionLimit)
		}
	})

	t.Run("a record this node's ended context cut is no acknowledgement", func(t *testing.T) {
		t.Parallel()
		rig, store, _ := owing(t)
		store.silent.Store(true)
		_, ev := rig.completion("t1")
		ended, end := context.WithCancel(t.Context())
		end()
		err := rig.coordinator.OnEvent(delivery(ended, ev), ev)
		var refused *TurnRefused
		if got := controlResult(err); errors.As(err, &refused) || got.Outcome == queue.OutcomeAck {
			t.Fatalf("the refusal whose record this node's ended context cut = %v, left %s; want it handed "+
				"back, the refusal not carried", err, got.Outcome)
		}
		if got := rig.get("t1"); got.Launch.RefusalOwed != "" {
			t.Fatalf("the job's record owes %q; want nothing recorded", got.Launch.RefusalOwed)
		}
	})

	t.Run("a row a newer lease owns", func(t *testing.T) {
		t.Parallel()
		rig, store, _ := owing(t)
		// THE SEAT'S NEXT HOLDER FENCED THE ROW, while this node's own lease
		// still says it holds the seat.
		if won, err := rig.pending.ClaimOwnership(t.Context(), "t1", "node-c:1", rigLease.Epoch+1); err != nil || !won {
			t.Fatalf("the premise: fencing the row to the next holder = %v, %v", won, err)
		}
		_, ev := rig.completion("t1")
		err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev)
		if got := controlResult(err); !errors.Is(err, ErrSeatNotHeld) || got.Outcome == queue.OutcomeAck {
			t.Fatalf("the refusal on a row a newer lease owns = %v, left %s; want it not acknowledged", err,
				got.Outcome)
		}
		if got := rig.get("t1"); got.Launch.RefusalOwed != "" || store.written.Load() != 0 {
			t.Fatalf("the job's record owes %q; want nothing written under the lost lease", got.Launch.RefusalOwed)
		}
	})

	t.Run("a lease gone before the record", func(t *testing.T) {
		t.Parallel()
		rig, store, _ := owing(t)
		payload, _ := rig.completion("t1")
		// THE GATE REFUSED, and this node let the seat go before the record.
		refused := fmt.Errorf("%w: %w", ErrResumeUnavailable, &TurnRefused{
			Refusal: Refusal{Condition: conditionLimit, Reason: "the account is at its limit"}})
		rig.coordinator.lease = notLeased
		err := rig.coordinator.oweRefused(t.Context(), payload, refused)
		if !errors.Is(err, ErrResumeElsewhere) || !errors.Is(err, ErrSeatNotHeld) {
			t.Fatalf("the refusal under a lease gone = %v; want it routed on as a seat this node "+
				"does not hold, which its holder can resume", err)
		}
		if got := rig.get("t1"); got.Launch.RefusalOwed != "" || store.written.Load() != 0 {
			t.Fatalf("the job's record owes %q after %d writes; want nothing written under a lease gone",
				got.Launch.RefusalOwed, store.written.Load())
		}
		if got := controlResult(rig.coordinator.lost(payload.AgentHandle, err)); got.Outcome != queue.OutcomeDefer {
			t.Fatalf("the refusal under a lease gone left %s; want it handed on to the seat's holder",
				got.Outcome)
		}
	})
}

// A DRAIN'S REFUSAL IS HANDED ON AND OWES NOTHING. A draining node starts no
// turn, and the condition does not clear on it, so the control topic hands the
// delivery on to the seat's next holder rather than acknowledging it, and a
// delivery handed on leaves nothing owed on the job's record. Recorded as an
// acknowledged refusal would be, a drain's cost a write per job its deferred
// delivery never needed, and one the store did not take came back as the
// store's silence, handed back to the node that is leaving delivery after
// delivery rather than on.
//
// Mutation: record a drain's refusal as an acknowledged one is, and its
// record is written, and where the store does not take it the delivery is
// handed back as the store's silence.
func TestADrainsRefusalIsHandedOnOwingNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		silent bool
	}{
		{name: "the store answering"},
		{name: "the store silent", silent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			unsettledHost(t, rig)
			store := &owedRefusals{PendingStore: rig.coordinator.pending}
			store.silent.Store(tc.silent)
			rig.coordinator.pending = store
			gate := &conditionGate{}
			gate.refuse(Draining)
			rig.coordinator.gate = gate.admit
			_, ev := rig.completion("t1")
			err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev)
			var refused *TurnRefused
			if got := controlResult(err); errors.Is(err, ErrStoreSilent) || !errors.As(err, &refused) ||
				refused.Condition != Draining || got.Outcome != queue.OutcomeDefer {
				t.Fatalf("the drain's refusal = %v, left %s; want it handed on to the seat's next holder as the "+
					"drain's", err, got.Outcome)
			}
			if got := rig.get("t1"); got.Launch.RefusalOwed != "" || store.written.Load() != 0 {
				t.Fatalf("the job's record owes %q after %d writes; want nothing written for a refusal handed on",
					got.Launch.RefusalOwed, store.written.Load())
			}
		})
	}
}

// A SETTLED COMPLETION OWES NO REFUSAL. The record says a holder acknowledged a
// refused delivery of the job's completion and owes it its re-fire; the write
// that settles the completion clears it: the park on the job's question here,
// in its own write, as the turn's next launch starts the record afresh and the
// run's end deletes the row. And a holder that recovers the seat re-fires
// nothing for a run whose completion settled.
//
// Mutation: leave the record standing at the park, and the parked run's record
// still owes its completion.
func TestASettledCompletionOwesNoRefusal(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.ask("t1", whichBranch())
	rig.runner.Finish(Result{})
	gate := &conditionGate{}
	gate.refuse(conditionLimit)
	rig.coordinator.gate = gate.admit
	_, ev := rig.completion("t1")
	if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); controlResult(err).Outcome != queue.OutcomeAck {
		t.Fatalf("the premise: the refused delivery = %v; want it acknowledged", err)
	}
	if got := rig.get("t1"); got.Launch.RefusalOwed != conditionLimit {
		t.Fatalf("the premise: the job's record owes %q; want %q", got.Launch.RefusalOwed, conditionLimit)
	}
	// THE CONDITION CLEARS, and the re-fire parks the run on its question.
	gate.refuse("")
	rig.coordinator.Readmit(conditionLimit, "swe")
	rig.coordinator.inflight.Wait()
	got := rig.get("t1")
	if got.Status != StatusAwaiting {
		t.Fatalf("the premise: the re-fire left the run %s; want it parked on its question", got.Status)
	}
	if got.Launch.RefusalOwed != "" || !got.Launch.RefusalOwedSince.IsZero() {
		t.Fatalf("the parked run's record still owes %q since %v; want the park to have settled it",
			got.Launch.RefusalOwed, got.Launch.RefusalOwedSince)
	}
	// AND A HOLDER THAT RECOVERS THE SEAT RE-FIRES NOTHING.
	next := &conditionGate{}
	rig.restart(t, rig.queue, "node-c", Fence{Owner: "node-c:1", Epoch: rigLease.Epoch + 1}, next.admit)
	if err := rig.recoverSeat(t.Context(), "node-c:1", rigLease.Epoch+1); err != nil {
		t.Fatalf("recovering the seat: %v", err)
	}
	rig.coordinator.inflight.Wait()
	if asked := next.count(); asked != 0 {
		t.Fatalf("the recovering holder asked its gate %d times; want nothing re-fired for a settled completion",
			asked)
	}
}
