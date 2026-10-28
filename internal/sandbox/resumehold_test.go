package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
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
	// THE RESUMED TURN RELAUNCHES ITS EXECUTOR, through the coordinator, as
	// the engine's launcher does: the relaunch counts the seat on the same row.
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
// gave its claim back, one its seat's budget held, and a panic on its way up.
//
// Mutation: drop the recount from any of those exits, and its case leaves the
// seat held on a run that no longer holds it; drop the held exit's uncount, and
// its case counts the one run twice.
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
		"a resume its budget held":          {err: budgetHeld(time.Time{}), held: true},
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
			// ANNOUNCED ONLY WHERE NO TURN RAN, as an abandoned resume is: a
			// turn that began publishes its own failed completion.
			broken := false
			for _, f := range rig.failures() {
				broken = broken || f.Reason == types.SandboxFailureResumeBroken
			}
			if broken != beforeTurn {
				t.Fatalf("resume_broken announced %v, want %v: it is the run's only account "+
					"where its turn never began, and a second one where it did", broken, beforeTurn)
			}
		})
	}
}

// AN ANSWER WHOSE RESUME ITS BUDGET HELD WAITS, UNCHARGED, as one refused for
// its models' limit does. A completion's held resume is held on its row; an
// answer's is its own series, and the hold its resumed turn met — the relaunch
// the answer carries the run on with, refused for a window full or too short
// for a launch — is the series' wait. A window lasts until it turns over, which
// can be the rest of the day; charged as failures, the answer's attempts are
// spent on the wait and the answer is let go of while nothing was wrong with it.
//
// Mutation: read only a [TurnRefused] as a wait (waitingRefusal), and the
// answer is declined before the window turns over.
func TestAnAnswerWhoseResumeItsBudgetHeldWaitsUncharged(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	answeredAndOwed(t, rig)
	rig.resumer.failWith(budgetHeld(rig.now.Add(time.Hour)))
	rig.resumer.failsInTurn = true

	for range 3 * MaxAnswerAttempts {
		if rig.fireRetries() != 1 {
			t.Fatal("a held resume stopped waiting")
		}
	}
	if got := rig.get("t1"); got.Status != StatusAnswered || got.Answer == nil {
		t.Fatalf("run = %q (answer %v), want the answer still recorded on it", got.Status, got.Answer)
	}

	rig.resumer.failWith(nil)
	rig.coordinator.Readmit("budget", "swe")
	rig.fireRetries()
	if calls := rig.resumer.calls(); len(calls) != 1 {
		t.Fatalf("%d resumes once the window turned over, want 1", len(calls))
	}
}

// A COMPLETION WHOSE RESUME MEETS A LIMIT IT CAN NAME THE END OF IS HELD UNTIL
// THEN, as well as kept for the signal that ends it. The gate can admit a resume
// whose relaunch then meets the limit (a review on a ready member of the chain,
// sending back to runners every one of which sits out); unheld, the waiter's
// next poll announced the job again, and the box was collected and the review
// run again, into the same refusal, every poll for as long as the limit lasted.
// The signal lifts the hold and re-fires the kept completion, once.
//
// Mutation: hand the claim back unheld, and the next poll announces the job.
func TestACompletionRefusedForALimitIsHeldUntilItsInstant(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	gated(rig)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
	until := rig.now.Add(time.Hour)
	rig.resumer.failWith(&TurnRefused{Cause: errors.New("llm chain exhausted: usage limit"),
		Refusal: Refusal{Condition: conditionLimit, Reason: "provider limit: default", Until: until}})
	rig.resumer.failsInTurn = true

	payload, ev := rig.completion("t1")
	var refused *TurnRefused
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); !errors.As(err, &refused) {
		t.Fatalf("OnCompleted = %v, want the resume's refusal", err)
	}
	got := rig.get("t1")
	if got.Status != StatusRunning || got.HeldFor != conditionLimit || !got.HeldUntil.Equal(until) {
		t.Fatalf("row = %s held for %q until %v, want running and held until %v",
			got.Status, got.HeldFor, got.HeldUntil, until)
	}
	if fired := rig.tick(); fired != 0 {
		t.Fatalf("the poll announced %d completions for a resume held on its limit", fired)
	}

	rig.resumer.failWith(nil)
	rig.coordinator.Readmit(conditionLimit, "swe")
	eventually(t, func() bool { return len(rig.resumer.calls()) == 1 },
		"the kept completion never resumed on its condition's signal")
}

// A JOB COLLECTED AGAIN IS READ AS IT WAS WHEN IT WAS FIRST COLLECTED. Its
// ending is handed to the resume with the instant it was first read back
// ([RunEnding.At]), because a CLI names a reset as a time of day: a resume held
// for hours and collected again, its ending read against that later instant,
// took a 4:30pm already past for the next day's, and sat the entry out for
// nothing. The first collection's instant rides the job's record through the
// hold ([LaunchRecord.CollectedAt]), and so does the CLI's own terminal event.
//
// Mutation: date the ending by the collection that reads it, and the second
// resume's ending carries the later instant.
func TestARecollectedJobsEndingIsDatedByItsFirstCollection(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	first := rig.now
	const event = `{"type":"result","is_error":true,"api_error_status":429,"result":"You've hit your 5-hour limit"}`
	rig.resumer.failWith(budgetHeld(first.Add(time.Hour)))
	rig.runner.Finish(Result{Error: "You've hit your 5-hour limit", Event: event})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("a held resume was NAKed: %v", err)
	}
	if got := rig.get("t1").Launch.CollectedAt; !got.Equal(first) {
		t.Fatalf("the job's record says it was first collected at %v, want %v", got, first)
	}

	rig.now = first.Add(2 * time.Hour)
	rig.resumer.failWith(nil)
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("the poll announced %d completions once the window turned, want the held job's", fired)
	}
	payload, ev = rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted once the hold lifted: %v", err)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times, want once", len(calls))
	}
	if ended := calls[0].Ended; !ended.At.Equal(first) || ended.Event != event {
		t.Fatalf("the second collection hands the resume an ending read at %v carrying %q, "+
			"want the first collection's %v and the CLI's own event", ended.At, ended.Event, first)
	}
}
