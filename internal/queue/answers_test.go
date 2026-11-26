package queue_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
)

// AN ANSWER PAST THE BOUND IS COUNTED AS HELD, and only while it is: queuetest's
// a_registration_answers_up_to_its_own_bound_and_holds_no_other reads the
// bound off this count, so one that missed a held answer, or kept an admitted
// or abandoned one, would certify a bound nothing holds. In a bubble, so the
// count is read once a held Enter is durably parked, never after a pause.
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
// bound that ends first is answered with that bound's error: every Serve and
// Listen withdrawal rests on it, or its caller tears down what a running
// answer reads. In a bubble, so a close that would return without waiting has
// returned before the case looks.
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

// A REQUEST PAST THE SLOTS WAITS IN ARRIVAL ORDER AND KEEPS ITS ARRIVAL: Offer
// never parks its caller, the gate starts each held request as a slot frees,
// first come first, and hands it the instant it arrived, so an answer counts
// its asker's wait from there and not from its slot. In a bubble, so the
// instants are exact.
//
// Mutation: hand a request the instant it starts, and the held ones read 10 s
// and 0 s late, red.
func TestARequestPastTheSlotsWaitsInArrivalOrderAndKeepsItsArrival(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g, err := queue.NewServeGate(1, 4, 1<<20)
		if err != nil {
			t.Fatalf("NewServeGate: %v", err)
		}
		release := make(chan struct{})
		got := make(chan string, 3)
		begun := time.Now()
		if err := g.Offer(time.Now(), 1, func(time.Time) { <-release }, nil); err != nil {
			t.Fatalf("an empty gate refused a request: %v", err)
		}
		for _, name := range []string{"second", "third"} {
			at := time.Now()
			if err := g.Offer(at, 1, func(arrived time.Time) {
				got <- fmt.Sprintf("%s arrived %v in", name, arrived.Sub(begun))
			}, nil); err != nil {
				t.Fatalf("a full gate refused %s: %v", name, err)
			}
			time.Sleep(5 * time.Second)
		}
		synctest.Wait()
		if running, waiting := g.Occupancy(); running != 1 || waiting != 2 {
			t.Fatalf("with one slot held and two requests offered the gate reports %d running and %d "+
				"waiting, want 1 and 2", running, waiting)
		}
		close(release)
		want := []string{"second arrived 0s in", "third arrived 5s in"}
		for _, w := range want {
			if ran := <-got; ran != w {
				t.Errorf("a held request ran as %q, want %q", ran, w)
			}
		}
	})
}

// A GATE DROPS PAST WHAT IT HOLDS WAITING, in count or in bytes, and says a
// burst once: the first drop since it last took a request is First, the rest
// are not, and taking one starts a new burst. The bound is what keeps a
// delivery that never parks from holding unbounded memory.
//
// Mutation: drop the byte bound, and the large request is held, red.
func TestAGateDropsPastWhatItHoldsWaitingInCountOrBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g, err := queue.NewServeGate(1, 2, 10)
		if err != nil {
			t.Fatalf("NewServeGate: %v", err)
		}
		release := make(chan struct{})
		hold := func(time.Time) { <-release }
		offer := func(size int) error { return g.Offer(time.Now(), size, hold, nil) }
		if err := offer(1); err != nil {
			t.Fatalf("an empty gate refused a request: %v", err)
		}
		if err := offer(7); err != nil {
			t.Fatalf("a request within both bounds was refused: %v", err)
		}
		var drop *queue.DroppedRequest
		if err := offer(4); !errors.As(err, &drop) || !drop.First || drop.WaitingBytes != 7 {
			t.Fatalf("a request past the byte bound = %v, want the first drop of a burst at 7 bytes", err)
		}
		if err := offer(4); !errors.As(err, &drop) || drop.First {
			t.Fatalf("a second drop in the same burst = %v, want a drop that is not the burst's first", err)
		}
		if err := offer(3); err != nil {
			t.Fatalf("a request within both bounds was refused: %v", err)
		}
		if err := offer(1); !errors.As(err, &drop) || !drop.First || drop.Waiting != 2 {
			t.Fatalf("a request past the count bound, after one was taken = %v, want the first drop of "+
				"a new burst at 2 requests", err)
		}
		close(release)
		synctest.Wait()
		if err := offer(1); err != nil {
			t.Fatalf("an empty gate refused a request: %v", err)
		}
		synctest.Wait()
		if running, waiting := g.Occupancy(); running != 0 || waiting != 0 {
			t.Fatalf("once every request ran the gate reports %d running and %d waiting", running, waiting)
		}
	})
}

// A WITHDRAWAL ANSWERS NOTHING IT HELD WAITING: once a gate is shut, a slot
// freed is given back rather than handed to the request waiting for it, a
// later offer is refused, and Close returns once the answers already running
// have left. Every Serve withdrawal rests on it, or a request queued before an
// Unsubscribe would be answered after it.
//
// Mutations: hand the slot on in Leave without checking the gate is shut, and
// the held request runs; drop it without telling its caller, red.
func TestAWithdrawalAnswersNothingItHeldWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g, err := queue.NewServeGate(1, 4, 1<<20)
		if err != nil {
			t.Fatalf("NewServeGate: %v", err)
		}
		release := make(chan struct{})
		var ran, dropped atomic.Int32
		if err := g.Offer(time.Now(), 1, func(time.Time) { ran.Add(1); <-release }, nil); err != nil {
			t.Fatalf("an empty gate refused a request: %v", err)
		}
		if err := g.Offer(time.Now(), 1, func(time.Time) { ran.Add(1) }, func() { dropped.Add(1) }); err != nil {
			t.Fatalf("a full gate refused a request: %v", err)
		}
		synctest.Wait()
		g.Shut()
		if n := dropped.Load(); n != 1 {
			t.Fatalf("the shut told %d dropped requests, want the one it held waiting", n)
		}
		if err := g.Offer(time.Now(), 1, func(time.Time) { ran.Add(1) }, nil); !errors.Is(err, queue.ErrGateClosed) {
			t.Fatalf("an offer to a shut gate = %v, want %v", err, queue.ErrGateClosed)
		}
		close(release)
		if err := g.Close(t.Context()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		synctest.Wait()
		if n := ran.Load(); n != 1 {
			t.Fatalf("%d requests ran, want only the one running before the withdrawal", n)
		}
		if running, waiting := g.Occupancy(); running != 0 || waiting != 0 {
			t.Fatalf("a closed gate reports %d running and %d waiting, want none", running, waiting)
		}
	})
}

// A SLOT FREED IS NEVER LOST TO A REQUEST QUEUED BESIDE IT: a one-slot gate
// hammered by concurrent offers, whose answers return at once, runs every one
// of them and holds none waiting once everything settles, round after round.
// A slot freed apart from the check that found nobody waiting leaves a request
// queued in between with a free slot and nobody to start it, and the gate
// wedged for good. Under -race, in a bubble per round, so "settled" is the
// bubble's own reading, never a pause.
//
// Mutation: free the slot in Leave after letting the lock go, and a round
// settles with a request still waiting, red.
func TestASlotFreedIsNeverLostToARequestQueuedBesideIt(t *testing.T) {
	const rounds, offers = 2000, 16
	for range rounds {
		synctest.Test(t, func(t *testing.T) {
			g, err := queue.NewServeGate(1, offers, 1<<20)
			if err != nil {
				t.Fatalf("NewServeGate: %v", err)
			}
			var ran atomic.Int32
			var wg sync.WaitGroup
			for range offers {
				wg.Go(func() {
					if err := g.Offer(time.Now(), 1, func(time.Time) { ran.Add(1) }, nil); err != nil {
						t.Errorf("a gate refused a request within its bound: %v", err)
					}
				})
			}
			wg.Wait()
			synctest.Wait()
			if running, waiting := g.Occupancy(); running != 0 || waiting != 0 || ran.Load() != offers {
				t.Fatalf("once every offer settled the gate had run %d of %d requests and reports %d "+
					"running and %d waiting: a request was left waiting beside a free slot", ran.Load(),
					offers, running, waiting)
			}
		})
	}
}
