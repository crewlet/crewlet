package sandbox

import (
	"context"
	"slices"
	"testing"
)

// A RESUMED RUN KEEPS ITS SEAT HELD FOR THE WHOLE RESUMED TURN, and lets go of
// it once the turn is over.
//
// What a running engine did: a coding job finished, its completion claimed the
// run (`resumed`, which holds the seat) and resumed the turn that launched it.
// The resume gave the seat's count back BEFORE the resumer ran, so the seat's
// inbox hold lifted for the length of the resumed turn, and the seat's next
// mail started a second turn beside it 444 ms later: that turn launched a
// second job, and the seat held two runs at once. A seat busy coding takes no
// other work (seathold.go), and the resumed turn is that work.
//
// Mutation: give the count back before the resumer runs, and the inbox is not
// held during the turn; skip the recount after it, and it is never let go of.
func TestAResumedRunKeepsItsSeatHeldThroughTheTurn(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	holds := rig.withHold()
	launchScheduled(t, rig, "t1")
	rig.coordinator.reconcileHold(t.Context(), "swe")
	if !holds.holding("swe") {
		t.Fatal("the premise: the running job holds the seat's inbox")
	}

	var during []bool
	var heldRuns []bool
	rig.resumer.during = func(context.Context, PendingRun) {
		during = append(during, holds.holding("swe"))
		held, _ := rig.coordinator.SeatRuns("swe")
		heldRuns = append(heldRuns, held)
	}
	rig.runner.Finish(Result{Success: true})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if !slices.Equal(during, []bool{true}) || !slices.Equal(heldRuns, []bool{true}) {
		t.Fatalf("during the resumed turn the inbox was held %v and the seat counted held %v, "+
			"want both throughout the one turn", during, heldRuns)
	}
	if holds.holding("swe") {
		t.Fatal("the seat's inbox is still held after the resumed turn ended and the run finished")
	}
	if held, _ := rig.coordinator.SeatRuns("swe"); held {
		t.Fatal("the seat still counts a run holding it after the run finished")
	}
}

// A RESUMED TURN THAT LAUNCHES THE NEXT JOB HOLDS THE SEAT ONCE, not twice:
// the claim's count and the relaunch's are one row, and a seat whose count
// stayed at two after the first of them settled would be held for good.
//
// Mutation: drop the recount from the relaunch branch, and the seat is still
// held after the relaunched job settles.
func TestAResumedTurnThatRelaunchesHoldsTheSeatOnce(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	holds := rig.withHold()
	launchScheduled(t, rig, "t1")
	// THE RESUMED TURN CALLS run_sandbox AGAIN, through the coordinator, as
	// the engine's tool does: the relaunch counts the seat on the same row.
	rig.resumer.during = func(ctx context.Context, run PendingRun) {
		if _, err := rig.coordinator.Launch(ctx, rig.manager, launchReq(run.TurnID)); err != nil {
			t.Errorf("relaunch: %v", err)
		}
		rig.suspendIn(ctx, run.TurnID)
	}
	rig.runner.Finish(Result{Success: true})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	rig.resumer.during = nil
	rig.coordinator.mu.Lock()
	counted := rig.coordinator.runs["swe"]
	rig.coordinator.mu.Unlock()
	if counted.holding+counted.awaiting != 1 {
		t.Fatalf("the seat counts %+v after the resumed turn relaunched, want the one run once",
			counted)
	}
	if got := rig.get("t1"); slices.Contains(Holding, got.Status) && !holds.holding("swe") {
		t.Fatal("the relaunched job does not hold the seat's inbox")
	}
}
