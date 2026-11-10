package queue_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/crewlet/crewlet/internal/queue"
)

// AN ANSWER PAST THE BOUND IS COUNTED AS HELD, and only while it is: the gate's
// own decision to hold it is what the conformance suite reads a registration's
// bound off (queuetest's
// a_registration_answers_up_to_its_own_bound_and_holds_no_other), so a count
// that missed it, kept it once it was admitted or kept one whose context ended
// would certify a bound nothing holds, or report one holding what it does not.
//
// In a bubble, where synctest.Wait returns once every other goroutine of the
// case is durably blocked: an Enter the gate holds is parked on its slots, so
// the count is read once the gate has decided, never after a pause.
//
// Mutation: drop the increment in Enter, and the held answer is reported as
// nothing.
func TestAnAnswerPastTheBoundIsCountedWhileTheGateHoldsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		g, err := queue.NewAnswerGate(1)
		if err != nil {
			t.Fatalf("NewAnswerGate: %v", err)
		}
		if !g.Enter(ctx) {
			t.Fatal("an empty gate refused an answer")
		}
		admitted := make(chan bool, 1)
		go func() { admitted <- g.Enter(ctx) }()
		synctest.Wait()
		if running, waiting := g.Occupancy(); running != 1 || waiting != 1 {
			t.Fatalf("with its one slot taken and a second answer arriving, the gate reports "+
				"%d running and %d held, want 1 and 1", running, waiting)
		}

		g.Leave()
		if !<-admitted {
			t.Fatal("a held answer was refused once the slot it waited for was free")
		}
		if running, waiting := g.Occupancy(); running != 1 || waiting != 0 {
			t.Fatalf("once the held answer was admitted, the gate reports %d running and %d "+
				"held, want 1 and 0", running, waiting)
		}

		// AND ONE WHOSE CONTEXT ENDS WHILE IT WAITS leaves the count with it.
		asker, giveUp := context.WithCancel(ctx)
		refused := make(chan bool, 1)
		go func() { refused <- g.Enter(asker) }()
		synctest.Wait()
		if _, held := g.Occupancy(); held != 1 {
			t.Fatalf("the gate reports %d answers held, want the one waiting for its slot", held)
		}
		giveUp()
		if <-refused {
			t.Fatal("an answer whose context ended while it waited for a slot was admitted")
		}
		if running, held := g.Occupancy(); running != 1 || held != 0 {
			t.Fatalf("once the waiting answer gave up, the gate reports %d running and %d held, "+
				"want 1 and 0", running, held)
		}
		g.Leave()
	})
}

// A CLOSE WAITS FOR THE ANSWERS IT ADMITTED, bounded by its context, and a
// bound that ends first is answered with that bound's error — the rule every
// withdrawal of Serve and Listen rests on, on both backends, through this one
// gate. A close that returned with an answer admitted would let its caller
// tear down what the answer reads; one that answered success when its bound
// ended would say so with the answer still running.
//
// In a bubble, so the close not having returned is read once every goroutine
// but the case's is durably blocked: a close that was going to return without
// waiting has returned by then.
//
// Mutation: return from Close without waiting while an answer is admitted, and
// the first half fails; answer an ended bound with nil, and the second does.
func TestAGateClosesOnceItsAnswersHaveLeftOrItsBoundEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		g, err := queue.NewAnswerGate(2)
		if err != nil {
			t.Fatalf("NewAnswerGate: %v", err)
		}
		if !g.Enter(ctx) {
			t.Fatal("an empty gate refused an answer")
		}
		closed := make(chan error, 1)
		go func() { closed <- g.Close(ctx) }()
		synctest.Wait()
		select {
		case err := <-closed:
			t.Fatalf("Close answered %v with an answer still admitted", err)
		default:
		}
		if g.Enter(ctx) {
			t.Fatal("a gate that is closing admitted an answer")
		}
		g.Leave()
		if err := <-closed; err != nil {
			t.Fatalf("Close answered %v once its one answer had left, want nil", err)
		}

		held, err := queue.NewAnswerGate(1)
		if err != nil {
			t.Fatalf("NewAnswerGate: %v", err)
		}
		if !held.Enter(ctx) {
			t.Fatal("an empty gate refused an answer")
		}
		ended, cancel := context.WithTimeout(ctx, 0)
		defer cancel()
		if err := held.Close(ended); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close under a bound that had ended, with an answer admitted, answered "+
				"%v, want the bound's own %v", err, context.DeadlineExceeded)
		}
		held.Leave()
	})
}
