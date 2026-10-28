package node_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/queue"
)

// THE ADMISSION OUTSIDE THE MAILBOX.
//
// A seat's mailbox is not the only road to a turn: a coding run that finishes
// resumes its turn from the seat's control topic, and a recorded answer's retry
// from the sandbox coordinator's own timer. A draining node went on starting
// full turns through both, past its concurrency ceiling, because only the
// mailbox's handler asked. These cases hold [node.Node.AdmitTurn] to the same
// answers the mailbox always got, for a caller that is not a delivery.

// idleTurn is a mailbox turn that does nothing, for a case about admissions
// made outside the mailbox.
func idleTurn(context.Context, string, []*events.Event) queue.Result { return queue.Ack() }

// admit admits a turn on handle or fails the case.
func admit(ctx context.Context, t *testing.T, n *node.Node, handle string) (context.Context, func()) {
	t.Helper()
	turnCtx, release, err := n.AdmitTurn(ctx, handle)
	if err != nil {
		t.Fatalf("AdmitTurn(%s) = %v, want admitted", handle, err)
	}
	return turnCtx, release
}

// admitting runs AdmitTurn off the test goroutine, for an admission expected
// to wait, and reports its answer when it comes.
func admitting(ctx context.Context, n *node.Node, handle string) <-chan error {
	answer := make(chan error, 1)
	go func() {
		_, release, err := n.AdmitTurn(ctx, handle)
		if err == nil {
			release()
		}
		answer <- err
	}()
	return answer
}

// A TURN ADMITTED OUTSIDE THE MAILBOX COUNTS AGAINST THE CEILING, and frees its
// slot when it is released, once. The turn behind it is counted as waiting
// ([node.Node.TurnsWaiting]) for as long as it is parked, and no longer.
//
// Mutation: admit without acquiring a slot and the second admission is not
// held; release without freeing it and the waiter is never admitted; leave
// the waiting count up after the wait and it is still one at the end.
func TestATurnAdmittedOutsideTheMailboxHoldsASlot(t *testing.T) {
	t.Parallel()
	g := gatedFleet(t, 1, idleTurn, "ceo", "cto")

	_, release := admit(t.Context(), t, g.n, "ceo")
	if got := g.n.InFlightTurns(); got != 1 {
		t.Fatalf("in flight = %d with one turn admitted, want 1", got)
	}
	waiting := admitting(t.Context(), g.n, "cto")
	within(t, "the second turn to park for the slot", func() bool { return g.n.TurnsWaiting() == 1 })
	select {
	case err := <-waiting:
		t.Fatalf("a second turn was answered (%v) under a ceiling of one", err)
	default:
	}

	release()
	release() // idempotent: a second release frees nothing it does not hold
	select {
	case err := <-waiting:
		if err != nil {
			t.Fatalf("the waiting turn was refused (%v) when the slot freed", err)
		}
	case <-time.After(gateWaitBound):
		t.Fatal("the waiting turn was not admitted when the slot freed")
	}
	within(t, "every slot to be free", func() bool { return g.n.InFlightTurns() == 0 })
	if got := g.n.TurnsWaiting(); got != 0 {
		t.Fatalf("turns waiting = %d once the waiting turn was admitted, want 0", got)
	}
}

// A SEAT THIS NODE DOES NOT HOLD ADMITS NOTHING, and says so by the sentinel a
// caller hands the work back on.
func TestASeatNotHeldHereAdmitsNoTurn(t *testing.T) {
	t.Parallel()
	g := gatedFleet(t, 2, idleTurn, "ceo")

	_, release, err := g.n.AdmitTurn(t.Context(), "nobody")
	if !errors.Is(err, node.ErrSeatNotOwned) {
		t.Fatalf("AdmitTurn on a seat this node does not hold = %v, want ErrSeatNotOwned", err)
	}
	if release != nil {
		t.Error("a refusal handed back a release for a slot it never took")
	}
	if got := g.n.InFlightTurns(); got != 0 {
		t.Errorf("in flight = %d after a refusal, want 0", got)
	}
}

// A DRAINING NODE ADMITS NO TURN, ON A SEAT IT STILL HOLDS.
//
// The window this is about: a drain waits for the turns already running, and
// keeps every seat while it does. Ownership still says yes there; the gate is
// what must say no, with the sentinel that tells a caller this node is leaving.
//
// Mutation: drop the drain's gate close, or check only ownership, and the
// admission during the drain is granted.
func TestADrainingNodeAdmitsNoTurn(t *testing.T) {
	t.Parallel()
	running, hold := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g := gatedFleet(t, 2, func(context.Context, string, []*events.Event) queue.Result {
		once.Do(func() { close(running) })
		<-hold
		return queue.Ack()
	}, "ceo", "cto")
	letGo := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(letGo)

	g.send("ceo", "keeps the drain waiting")
	select {
	case <-running:
	case <-time.After(3 * fleetTTL):
		t.Fatal("the turn holding the drain open never started")
	}
	drained := make(chan struct{})
	go func() {
		g.n.Drain(context.WithoutCancel(t.Context()))
		close(drained)
	}()

	var refusal error
	within(t, "the drain to close the gate", func() bool {
		_, release, err := g.n.AdmitTurn(t.Context(), "cto")
		if err == nil {
			release()
			return false
		}
		refusal = err
		return true
	})
	if !errors.Is(refusal, node.ErrDraining) {
		t.Fatalf("AdmitTurn during the drain = %v, want ErrDraining", refusal)
	}
	if held := g.n.Host().Held(); len(held) != 2 {
		t.Fatalf("the premise: a drain still waiting holds every seat, holds %v", held)
	}

	letGo()
	select {
	case <-drained:
	case <-time.After(10 * fleetTTL):
		t.Fatal("the drain never completed")
	}
}

// A DRAIN WAITS FOR A TURN NO DELIVERY STARTED.
//
// A recorded answer's retried resume runs on the sandbox coordinator's timer,
// in no queue handler: a drain that waited only for the queue's handlers
// handed the seat back to a peer while that turn was still running on it.
//
// Mutation: wait only for the queue's handlers in the drain and it returns
// with the turn still admitted.
func TestADrainWaitsForATurnNoDeliveryStarted(t *testing.T) {
	t.Parallel()
	g := gatedFleet(t, 2, idleTurn, "ceo")

	_, release := admit(t.Context(), t, g.n, "ceo")
	defer release()
	drained := make(chan struct{})
	go func() {
		g.n.Drain(context.WithoutCancel(t.Context()))
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("the drain handed the seats back while a turn was still running on one")
	case <-time.After(200 * time.Millisecond):
	}
	if held := g.n.Host().Held(); len(held) != 1 {
		t.Fatalf("the drain gave a seat back under a running turn: holds %v", held)
	}

	release()
	select {
	case <-drained:
	case <-time.After(10 * fleetTTL):
		t.Fatal("the drain never completed once the turn ended")
	}
}

// A TURN INSIDE A TURN REUSES ITS ADMISSION, and only while it is held.
//
// The inline answer resume runs on the mailbox delivery that carried the
// answer, which already holds the seat's slot. A second slot for the same turn
// would wait for ever under a ceiling of one: the slot it waits on is its own.
// And a context that outlives its turn keeps its values, so a released
// admission it still carries must admit nothing, or a turn started from it
// later would walk past a drain.
//
// Mutation: drop the nested check and the inner admission waits on its own
// slot; drop the released flag from it and the detached context is admitted
// without a slot.
func TestATurnInsideATurnReusesItsAdmission(t *testing.T) {
	t.Parallel()
	g := gatedFleet(t, 1, idleTurn, "ceo", "cto")

	outer, release := admit(t.Context(), t, g.n, "ceo")
	inner := admitting(outer, g.n, "ceo")
	select {
	case err := <-inner:
		if err != nil {
			t.Fatalf("the nested admission on the same seat = %v, want admitted", err)
		}
	case <-time.After(gateWaitBound):
		t.Fatal("the nested admission waited on the slot its own turn holds")
	}
	if got := g.n.InFlightTurns(); got != 1 {
		t.Fatalf("in flight = %d after a nested admission, want the one slot", got)
	}

	// ANOTHER SEAT is another turn, and waits for a slot like any, though
	// it asks under a context carrying this one's admission. Detached from
	// the outer turn's end, so it is still asking once that turn releases.
	detached := context.WithoutCancel(outer)
	other := admitting(detached, g.n, "cto")
	select {
	case err := <-other:
		t.Fatalf("a turn on another seat was answered (%v) inside a full gate", err)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	select {
	case err := <-other:
		if err != nil {
			t.Fatalf("the other seat's turn was refused (%v) once the slot freed", err)
		}
	case <-time.After(gateWaitBound):
		t.Fatal("the other seat's turn was not admitted once the slot freed")
	}

	// RELEASED: the context still carries the admission, and it must take a
	// slot of its own now.
	_, again := admit(detached, t, g.n, "ceo")
	if got := g.n.InFlightTurns(); got != 1 {
		t.Fatalf("in flight = %d for a context whose admission was released, "+
			"want a slot of its own", got)
	}
	again()
}

// THE DRAIN'S CUT ENDS A TURN ADMITTED OUTSIDE THE MAILBOX, as it ends a
// mailbox turn: once the acknowledgements are lost, its work would be run
// again by the seat's next holder.
//
// Mutation: drop the cut watcher from AdmitTurn and the admitted context is
// never cancelled.
func TestTheDrainsCutEndsATurnAdmittedOutsideTheMailbox(t *testing.T) {
	t.Parallel()
	acks := make(chan struct{})
	g := gatedFleetOn(t, 2, func(q queue.EventQueue) queue.EventQueue {
		return losingQueue{EventQueue: q, acks: acks}
	}, idleTurn, "ceo")

	turn, release := admit(t.Context(), t, g.n, "ceo")
	defer release()
	close(acks)
	drained := make(chan struct{})
	go func() {
		g.n.Drain(context.WithoutCancel(t.Context()))
		close(drained)
	}()
	select {
	case <-turn.Done():
	case <-time.After(cutBound):
		t.Fatal("the drain was cut short and the admitted turn was never cancelled")
	}
	select {
	case <-drained:
	case <-time.After(cutBound):
		t.Fatal("the drain cut short never returned")
	}
}

// gateWaitBound is how long a case waits for an admission that should already
// have been answered: it only elapses on a failure.
const gateWaitBound = 2 * time.Second

// A SEAT RUNS ONE TURN AT A TIME, whichever road each arrives by, and says so
// when the one running ends.
//
// The road this is about: a person's answer reaches the run it names on the
// seat's control topic, which no mailbox turn waits for, so its resume used to
// be able to start while the seat's mailbox was in the middle of a turn. The
// second turn is refused at once with the sentinel its caller waits on, never
// queued behind the first, and the seat's turn ending is announced so the
// waiting work tries again then rather than on a timer. A turn inside the
// running one is the same turn, and another seat is another turn.
//
// Mutation: drop the per-seat check and the second admission is granted; drop
// the hook call from the release and the ending is never announced.
func TestASeatRunsOneTurnAtATime(t *testing.T) {
	t.Parallel()
	ended := make(chan string, 4)
	g := gatedFleetWith(t, 4, nil, func(cfg *node.Config) {
		cfg.SeatTurnEnded = func(handle string) { ended <- handle }
	}, idleTurn, "ceo", "cto")

	running, release := admit(t.Context(), t, g.n, "ceo")
	if _, second, err := g.n.AdmitTurn(t.Context(), "ceo"); !errors.Is(err, node.ErrSeatBusy) {
		if second != nil {
			second()
		}
		t.Fatalf("a second turn on a seat mid-turn = %v, want ErrSeatBusy", err)
	}
	if got := g.n.InFlightTurns(); got != 1 {
		t.Fatalf("in flight = %d after a busy refusal, want the one turn's slot", got)
	}
	_, nested := admit(running, t, g.n, "ceo")
	nested()
	_, other := admit(t.Context(), t, g.n, "cto")
	other()
	select {
	case handle := <-ended:
		if handle != "cto" {
			t.Fatalf("the ending announced is %q, want cto's: ceo's turn is still running", handle)
		}
	case <-time.After(gateWaitBound):
		t.Fatal("the end of cto's turn was not announced")
	}

	release()
	select {
	case handle := <-ended:
		if handle != "ceo" {
			t.Fatalf("the ending announced is %q, want ceo's", handle)
		}
	case <-time.After(gateWaitBound):
		t.Fatal("the end of ceo's turn was not announced")
	}
	_, again := admit(t.Context(), t, g.n, "ceo")
	again()
}
