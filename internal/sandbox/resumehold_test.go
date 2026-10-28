package sandbox

import (
	"context"
	"errors"
	"fmt"
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

// EVERY WAY A RESUME ENDS LETS GO OF THE SEAT AS THE STORE SAYS, with no count
// left behind and none doubled. The count is kept through the resumed turn
// rather than given back before it, so each exit has to recount: a turn that
// broke after acting, one that broke before it began, a resume that failed and
// gave its claim back, and a panic on its way up.
//
// Mutation: drop the recount from any of those exits, and its case leaves the
// seat held on a run that no longer holds it.
func TestEveryResumeExitLetsGoOfTheSeat(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err        error
		beforeTurn bool
		// held is what the store says once the resume is over: a claim given
		// back to `running` still holds the seat, every other exit ends it.
		held bool
	}{
		"a turn that broke after acting":    {err: fmt.Errorf("%w: the provider went away", ErrResumeAbandoned)},
		"a turn that broke before it began": {err: fmt.Errorf("%w: a panic re-entering", ErrResumeAbandoned), beforeTurn: true},
		"a resume that gave its claim back": {err: errors.New("the model provider did not answer"), held: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			holds := rig.withHold()
			launchScheduled(t, rig, "t1")
			rig.resumer.failWith(tc.err)
			rig.resumer.beforeTurn = tc.beforeTurn
			rig.runner.Finish(Result{Success: true})
			payload, ev := rig.completion("t1")
			_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
			rig.coordinator.mu.Lock()
			counted := rig.coordinator.runs["swe"]
			rig.coordinator.mu.Unlock()
			want := 0
			if tc.held {
				want = 1
			}
			if counted.holding != want || holds.holding("swe") != tc.held {
				t.Fatalf("after the resume the seat counts %d holding (inbox held %v), want %d (%v)",
					counted.holding, holds.holding("swe"), want, tc.held)
			}
		})
	}
	for name, beforeTurn := range map[string]bool{
		"a panic after the turn began":  false,
		"a panic before the turn began": true,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			holds := rig.withHold()
			launchScheduled(t, rig, "t1")
			rig.coordinator.reconcileHold(t.Context(), "swe")
			if beforeTurn {
				rig.resumer.beforeBegin = func(context.Context) { panic("the resumer broke") }
			} else {
				rig.resumer.during = func(context.Context, PendingRun) { panic("the resumer broke") }
			}
			rig.runner.Finish(Result{Success: true})
			payload, ev := rig.completion("t1")
			func() {
				defer func() {
					if recover() == nil {
						t.Error("the panic was swallowed rather than passed on")
					}
				}()
				_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
			}()
			// THE PROCESS LIVES ON, as it does behind the queue's handler: the
			// run is ended rather than left in the claim, and the seat is free.
			if run, found, err := rig.pending.Get(t.Context(), "t1"); err != nil ||
				(found && run.Status == StatusResumed) {
				t.Fatalf("after a panic the run is %q (found %v, %v), want it ended, not left "+
					"in the claim", run.Status, found, err)
			}
			if held, _ := rig.coordinator.SeatRuns("swe"); held || holds.holding("swe") {
				t.Fatalf("after a panic the seat is still held (counted %v, inbox %v)",
					held, holds.holding("swe"))
			}
		})
	}
}
