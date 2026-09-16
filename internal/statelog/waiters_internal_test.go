package statelog

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A WAITER ON AN APPLIER THAT ENDS IS TOLD, NOT RELEASED.
//
// The applier used to close every waiter's own channel on the way out — the
// same channel a commit closes when it reaches the target — so a caller
// blocked on a position the loop never reached returned nil, read rows below
// its position, and reported the answer at the level it had asked for. A
// shutdown made that a footnote; a loop ended and started again around an
// adoption makes it a wrong answer on a running node.
func TestAWaiterOnAnEndedApplierIsToldRatherThanReleased(t *testing.T) {
	t.Parallel()
	var w waiters
	reached := func() Position { return Position{Stream: "s", Generation: 1, Seq: 5} }
	target := Position{Stream: "s", Generation: 1, Seq: 9}

	errs := make(chan error, 1)
	go func() { errs <- w.awaitPosition(context.Background(), target, reached) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && w.len() == 0 {
		time.Sleep(time.Millisecond)
	}
	if w.len() != 1 {
		t.Fatal("the waiter never registered")
	}

	w.abandonAll()
	select {
	case err := <-errs:
		if !errors.Is(err, ErrWaitAbandoned) {
			t.Fatalf("an abandoned wait returned %v, want %v — nil here is a "+
				"caller reading rows below the position it waited for", err, ErrWaitAbandoned)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned waiter is still waiting")
	}
	if w.len() != 0 {
		t.Fatalf("%d waiter(s) remain after the applier ended", w.len())
	}

	// AND A SATISFIED ONE IS STILL RELEASED, so the two paths are the two
	// answers they claim to be.
	go func() { errs <- w.awaitPosition(context.Background(), target, reached) }()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && w.len() == 0 {
		time.Sleep(time.Millisecond)
	}
	w.release(target)
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("a satisfied wait returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the satisfied waiter is still waiting")
	}
}
