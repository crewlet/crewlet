package sandbox

import (
	"errors"
	"testing"
	"time"
)

// WHAT AN EXPORT SPENDS MEASURING IS NOT WAITING. An export's allowance of
// waiting for the budget is for waiting: under load a measure of a large
// export takes a while, and a deadline set when the export arrived expired
// during it, so a receiver with room to spare answered busy — measured under
// the race detector on a loaded runner, refusing an export nothing else held
// the budget against. And the allowance is one for the whole export, however
// many times it grows.
//
// Mutation: set a deadline when the hold is made and the first case answers
// busy; give each growth its own allowance and the second waits twice.
func TestOnlyWaitingSpendsAnExportsWaitAllowance(t *testing.T) {
	t.Parallel()
	b := newOtelBudget(100)
	b.wait = 50 * time.Millisecond

	h := b.hold()
	time.Sleep(2 * b.wait) // a long measure
	if err := h.to(t.Context(), 60); err != nil {
		t.Fatalf("a hold took longer to measure than it may wait, with the budget free: %v", err)
	}

	other := b.hold()
	started := time.Now()
	if err := other.to(t.Context(), 30); err != nil {
		t.Fatal(err)
	}
	if err := other.to(t.Context(), 50); !errors.Is(err, ErrOtelBusy) {
		t.Fatalf("a growth past the budget: %v, want ErrOtelBusy", err)
	}
	if err := other.to(t.Context(), 60); !errors.Is(err, ErrOtelBusy) {
		t.Fatalf("a second growth past the budget: %v, want ErrOtelBusy", err)
	}
	if waited := time.Since(started); waited > 3*b.wait/2 {
		t.Errorf("two growths waited %v together, past the one allowance of %v", waited, b.wait)
	}

	// AND WHAT A HOLD GIVES BACK IS THERE FOR THE NEXT.
	h.release()
	if err := other.to(t.Context(), 70); err != nil {
		t.Errorf("a growth after the budget was given back: %v", err)
	}
	other.release()
	if !b.sem.TryAcquire(100) {
		t.Error("a released hold kept part of the budget")
	}
}
