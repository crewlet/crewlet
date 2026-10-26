package sandbox

import (
	"errors"
	"testing"
	"time"
)

// budgetHeld is the hold a resumed turn answers with when its seat's budget
// refused it: the window that refuses it ends at until.
func budgetHeld(until time.Time) error {
	return &HeldError{
		Refusal: Refusal{Condition: "budget", Reason: "budget: day window full", Until: until},
		Err:     errors.New("an executor run needs at least 25000 tokens"),
	}
}

// A RESUME ITS SEAT'S BUDGET REFUSED IS HELD UNTIL THE WINDOW TURNS OVER. The
// relaunch the turn's reviewer asked for was refused, so the job is handed back
// collected — the completion spent, never NAKed — with its resume held: the
// poll announces nothing for it while the hold lasts, and announces it again
// the moment the window has turned over. Handed back for an ordinary retry,
// the broker redelivered the completion and the poll re-announced it at once,
// and every attempt ran the review again into the same refusal.
//
// Mutation: treat the hold as an ordinary failure and the completion is NAKed;
// poll a held row and the next tick announces it at once.
func TestAResumeTheBudgetRefusedIsHeldUntilItsWindowTurns(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	resets := rig.now.Add(time.Hour)
	rig.resumer.failWith(budgetHeld(resets))
	rig.runner.Finish(Result{Success: true, Text: "first pass"})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("a held resume was NAKed: %v", err)
	}
	got := rig.get("t1")
	if got.Status != StatusRunning || got.HeldFor != "budget" || !got.HeldUntil.Equal(resets) {
		t.Fatalf("row = %s held for %q until %v, want running and held until %v",
			got.Status, got.HeldFor, got.HeldUntil, resets)
	}
	rig.resumer.failWith(nil)
	if fired := rig.tick(); fired != 0 {
		t.Fatalf("the poll announced %d completions for a resume still held", fired)
	}
	rig.now = resets.Add(time.Second)
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("the poll announced %d completions once the window turned, want the held job's", fired)
	}
}

// A SIGNAL FOR THE CONDITION LIFTS THE HOLD AT ONCE: a revision raising the
// seat's ceiling need not wait out the window it would have waited for.
func TestReadmittingTheBudgetLiftsAHeldResume(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.resumer.failWith(budgetHeld(rig.now.Add(24 * time.Hour)))
	rig.runner.Finish(Result{Success: true, Text: "first pass"})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	rig.resumer.failWith(nil)
	rig.coordinator.Readmit("budget", "swe")
	eventually(t, func() bool { return rig.get("t1").HeldFor == "" }, "the hold to lift")
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("the poll announced %d completions after the hold lifted, want the job's", fired)
	}
}
