package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
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
// What a running engine did: an executor's job finished, its completion
// claimed the run (`resumed`, which holds the seat) and resumed the turn that
// launched it. The resume gave the seat's count back BEFORE the resumer ran,
// so the seat's inbox hold lifted for the length of the resumed turn, and the
// seat's next mail started a second turn beside it 444 ms later: that turn
// launched a second job, and the seat held two runs at once. A seat busy
// coding takes no other work (seathold.go), and the resumed turn is that work.
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
				broken = broken || f.Reason == types.ExecutorFailureResumeBroken
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

// limitedAfterClaim is a rig whose swe run has finished and whose resume met
// its models' limit after its claim, until until: the review ran on a ready
// member of the chain and sent the run back to runners every one of which sits
// out, which the gate, asked again, would admit into again. The case gets the
// completion's payload and event back, as the waiter announces them.
func limitedAfterClaim(t *testing.T, rig *coordRig, until time.Time) (types.ExecutorFinished, *events.Event) {
	t.Helper()
	gated(rig)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
	rig.resumer.failWith(&TurnRefused{Cause: errors.New("llm chain exhausted: usage limit"),
		Refusal: Refusal{Condition: conditionLimit, Reason: "provider limit: default", Until: until}})
	rig.resumer.failsInTurn = true

	payload, ev := rig.completion("t1")
	var refused *TurnRefused
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); !errors.As(err, &refused) {
		t.Fatalf("OnCompleted = %v, want the resume's refusal", err)
	}
	if begun := rig.resumer.turnsBegun(); begun != 1 {
		t.Fatalf("the premise: %d resumed turns began, want the one that met the limit", begun)
	}
	return payload, ev
}

// A COMPLETION WHOSE RESUME MEETS A LIMIT IT CAN NAME THE END OF IS HELD BY ITS
// NODE UNTIL THEN, as well as kept for the signal that ends it. The gate
// admitted the resume and would admit it again; unheld, every announcement of
// the job collected its box and ran the review again, into the same refusal,
// for as long as the limit lasted. So this node refuses the waiter's
// announcements itself, before its gate and its claim, until the instant: the
// signal that ends the wait re-fires the kept completion once, and past the
// instant the next announcement is asked of the gate again.
//
// Mutation: drop the node's own check from OnCompleted, and the announcement
// made inside the window is claimed and resumed into the same limit.
func TestACompletionRefusedForALimitIsHeldHereUntilItsInstant(t *testing.T) {
	t.Parallel()
	for _, lift := range []string{"its signal", "its instant"} {
		t.Run(lift, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			until := rig.now.Add(time.Hour)
			payload, ev := limitedAfterClaim(t, rig, until)

			if fired := rig.tick(); fired != 1 {
				t.Fatalf("the poll announced %d completions, want the job's: the hold is not on its row", fired)
			}
			err := rig.coordinator.OnCompleted(t.Context(), payload, ev)
			var refused *TurnRefused
			if !errors.Is(err, ErrResumeUnavailable) || !errors.As(err, &refused) ||
				refused.Condition != conditionLimit || !refused.Until.Equal(until) {
				t.Fatalf("an announcement inside the window = %v, want this node's refusal "+
					"until %v", err, until)
			}
			if begun := rig.resumer.turnsBegun(); begun != 1 {
				t.Fatalf("an announcement inside the window was resumed: %d resumed turns, want 1", begun)
			}

			rig.resumer.failWith(nil)
			switch lift {
			case "its signal":
				rig.coordinator.Readmit(conditionLimit, "swe")
				eventually(t, func() bool { return len(rig.resumer.calls()) == 1 },
					"the kept completion never resumed on its condition's signal")
				rig.coordinator.inflight.Wait()
				if calls := rig.resumer.calls(); len(calls) != 1 {
					t.Fatalf("the signal resumed the completion %d times, want once", len(calls))
				}
			case "its instant":
				rig.now = until.Add(time.Second)
				if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
					t.Fatalf("the announcement past the instant: %v", err)
				}
				if calls := rig.resumer.calls(); len(calls) != 1 {
					t.Fatalf("%d resumes past the instant, want the announcement's", len(calls))
				}
			}
		})
	}
}

// A JOB READ BACK AND HANDED BACK KEEPS ITS BOX ASLEEP UNTIL IT IS COLLECTED
// AGAIN. The collection read the job and paused its box, and the resume then
// gave the claim back to running: held by its node for a model's limit, or
// handed back for a retry. The waiter goes on announcing the job at every poll,
// and the job is over, so it leaves the box as the collection left it: neither
// woken nor kept alive, until the collection that next claims the job wakes it
// to read it. Woken by the poll, a job its node held for a limit had its box
// resumed and kept alive at every poll for as long as the hold lasted, up to
// the hour a vendor's window is benched for: a running E2B sandbox, or an
// unpaused container, for work that was already done.
//
// Mutation: drop the read-back case from the waiter's reach, and the box is
// woken and kept alive at the first poll.
func TestAReadBackJobsBoxSleepsUntilItIsCollectedAgain(t *testing.T) {
	t.Parallel()
	for name, handBack := range map[string]func(t *testing.T, rig *coordRig){
		"held here for a limit": func(t *testing.T, rig *coordRig) {
			limitedAfterClaim(t, rig, rig.now.Add(time.Hour))
		},
		"handed back for a retry": func(t *testing.T, rig *coordRig) {
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
			rig.resumer.failWith(errors.New("the model provider did not answer"))
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
				t.Fatal("the premise: the resume failed and handed its claim back")
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			handBack(t, rig)
			run := rig.get("t1")
			box := rig.provider.Box(run.SandboxID)
			if run.Status != StatusRunning || !run.Launch.Collected || !box.Paused() {
				t.Fatalf("the premise: run %q (read back %v), box paused %v; want it running, "+
					"read back, its box paused", run.Status, run.Launch.Collected, box.Paused())
			}
			keepalives := box.Keepalives()
			for range 3 {
				if fired := rig.tick(); fired != 1 {
					t.Fatalf("the poll announced %d completions, want the job's", fired)
				}
			}
			if !box.Paused() || box.Keepalives() != keepalives {
				t.Fatalf("after three polls the box is paused %v and was kept alive %d times, "+
					"want it left asleep", box.Paused(), box.Keepalives()-keepalives)
			}

			rig.resumer.failWith(nil)
			rig.coordinator.Readmit(conditionLimit, "swe")
			payload, ev := rig.completion("t1")
			_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
			rig.coordinator.inflight.Wait()
			if calls := rig.resumer.calls(); len(calls) != 1 || calls[0].Ended.Text != "fixed the flake" {
				t.Fatalf("resumed %d times, want the job read back once more and resumed once", len(calls))
			}
		})
	}
}

// A PERSON'S STOP ENDS THE WAIT OF A COLLECTED JOB'S RESUME. The stop leaves a
// job a collection read back to its turn, whose fence closes it as stopped at
// its first round; but a resume held for a model's limit waits up to the hour
// the entry is sat out, and one held for the budget until its window turns
// over, and until then the turn stayed open and its job neither resumed nor
// ended. So the stop ends the wait: a completion its node held is re-fired at
// once, and a hold on the row is lifted for the waiter's next poll. The turn
// resumes now, inside the window, for its fence to close.
//
// Mutation: leave the wait to the condition in StopSeat, and the held job is
// not resumed before its instant.
func TestAStopEndsTheWaitOfACollectedJobsResume(t *testing.T) {
	t.Parallel()
	for name, held := range map[string]func(t *testing.T, rig *coordRig) (types.ExecutorFinished, *events.Event){
		"held here for a limit": func(t *testing.T, rig *coordRig) (types.ExecutorFinished, *events.Event) {
			return limitedAfterClaim(t, rig, rig.now.Add(time.Hour))
		},
		"held on its row for the budget": func(t *testing.T, rig *coordRig) (types.ExecutorFinished, *events.Event) {
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			rig.resumer.failWith(budgetHeld(rig.now.Add(time.Hour)))
			rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("a held resume was NAKed: %v", err)
			}
			if got := rig.get("t1"); got.HeldFor != "budget" {
				t.Fatalf("the premise: the row is held for %q, want the budget", got.HeldFor)
			}
			return payload, ev
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			payload, ev := held(t, rig)
			rig.resumer.failWith(nil)
			if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "ana", ByKind: "human"}); err != nil {
				t.Fatalf("StopSeat: %v", err)
			}
			// THE WAITER'S NEXT POLL, inside the window the wait was for.
			if rig.tick() == 1 {
				_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
			}
			rig.coordinator.inflight.Wait()
			if calls := rig.resumer.calls(); len(calls) != 1 {
				t.Fatalf("the stopped seat's held job resumed %d times inside its window, want once, "+
					"for its turn's fence to close", len(calls))
			}
		})
	}
}

// A STOP THAT LANDS WHILE A RESUME RUNS IS NOT KEPT OUT BY THAT RESUME'S HOLD.
// The stop leaves a claimed run to its turn, and the turn's call then comes back
// refused for a limit after the claim: kept and held, the job's announcements
// were refused here until the limit's instant, up to the hour an entry is
// benched for, while the stop stood unanswered. The stop is signalled for its
// seat, so the keep re-fires the completion at once, and the turn it resumes is
// one the seat's fence closes as stopped.
//
// Mutation: keep the refusal without asking whether a stop landed since
// (keepRefused), and the job is held until the instant.
func TestAStopThatLandsWhileAResumeRunsIsNotKeptOutByItsHold(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	gated(rig)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
	rig.resumer.failWith(&TurnRefused{Cause: errors.New("llm chain exhausted: usage limit"),
		Refusal: Refusal{Condition: conditionLimit, Reason: "provider limit: default",
			Until: rig.now.Add(time.Hour)}})
	rig.resumer.failsInTurn = true
	var once sync.Once
	rig.resumer.during = func(ctx context.Context, _ PendingRun) {
		once.Do(func() {
			// THE STOP LANDS while the resume's call is out: the run is
			// claimed, so the stop leaves it to the turn.
			if err := rig.coordinator.StopSeat(ctx, "swe", RunStop{By: "ana", ByKind: "human"}); err != nil {
				t.Errorf("StopSeat: %v", err)
			}
			rig.resumer.failWith(nil)
		})
	}

	payload, ev := rig.completion("t1")
	var refused *TurnRefused
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); !errors.As(err, &refused) {
		t.Fatalf("OnCompleted = %v, want the resume's refusal", err)
	}
	eventually(t, func() bool { return len(rig.resumer.calls()) == 1 },
		"the completion refused as the stop landed was kept until its instant")
	rig.coordinator.inflight.Wait()
	if _, held := rig.coordinator.stillRefused(payload); held {
		t.Error("the stopped seat's job is still held here")
	}
}

// A LIMIT A RESUME MET IS ITS NODE'S ALONE, AND SO IS THE HOLD. The models'
// sit-out is the node's own, which no peer shares and a restart forgets
// (ADR-0064), so the hold is kept in the node's memory and never written to
// the run's row, which every node reads: written there, it silenced the job
// for whoever held the seat next, a restarted node or the peer the seat moved
// to, whose own models may know of no limit at all, until the instant — up to
// the hour a vendor's window is benched for. Here the seat moves on inside the
// window, and its next holder resumes the job on the waiter's next
// announcement.
//
// Mutation: write the refusal onto the row as a hold (unclaimHeld), and the
// waiter announces nothing, and the next holder resumes nothing, until the
// instant.
func TestALimitAResumeMetIsHeldByItsNodeAlone(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	until := rig.now.Add(time.Hour)
	limitedAfterClaim(t, rig, until)
	if got := rig.get("t1"); got.Status != StatusRunning || got.HeldFor != "" || got.Held(rig.now) {
		t.Fatalf("row = %s held for %q until %v, want it running and unheld: the limit is "+
			"the resuming node's models'", got.Status, got.HeldFor, got.HeldUntil)
	}

	next := successorOf(t, rig, rig.pending)
	next.coordinator.manager = rig.manager
	gated(next)
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("the poll announced %d completions inside the first node's window, want the job's", fired)
	}
	payload, ev := rig.completion("t1")
	if err := next.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the next holder's OnCompleted inside the first node's window: %v", err)
	}
	if calls := next.resumer.calls(); len(calls) != 1 {
		t.Fatalf("the next holder resumed %d times inside the first node's window, want once", len(calls))
	}
}

// A LIMIT A RESUME MET IS NOT HELD FOR A SEAT ITS NODE LET GO OF MEANWHILE. The
// seat's release waits for no resume, and a resume refused after its claim
// returns once its turn has run: kept after the release had dropped what the
// node kept for the seat, the hold outlived the seat here, and the seat, back on
// this node inside the limit's window, had its job's announcements refused
// until the instant, by a hold its release was to have forgotten.
//
// Mutation: keep the completion without asking for the seat's lease again
// (keepRefused), and the announcement inside the window is refused here.
func TestALimitHeldForASeatLetGoMeanwhileIsForgotten(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	// THE SEAT IS LET GO OF WHILE THE RESUME RUNS, as a lost lease or a
	// rebalance lets it go: its lease gone, then what was kept for it dropped.
	rig.resumer.during = func(context.Context, PendingRun) {
		rig.coordinator.lease = notLeased
		rig.coordinator.ReleaseSeat("swe")
	}
	limitedAfterClaim(t, rig, rig.now.Add(time.Hour))

	// AND COMES BACK, inside the limit's window, to models that answer.
	rig.resumer.during = nil
	rig.coordinator.lease = leased(rigLease)
	rig.resumer.failWith(nil)
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the announcement once the seat is back: %v, want it resumed: the release was "+
			"to have forgotten the hold", err)
	}
	if calls := rig.resumer.calls(); len(calls) != 1 {
		t.Fatalf("resumed %d times once the seat was back, want once", len(calls))
	}
}

// A JOB COLLECTED AGAIN IS READ AS IT WAS WHEN IT WAS FIRST COLLECTED. Its
// ending is handed to the resume with the instant it was first read back
// ([RunEnding.At]), because a harness names a reset as a time of day: a resume held
// for hours and collected again, its ending read against that later instant,
// took a 4:30pm already past for the next day's, and sat the entry out for
// nothing. The first collection's instant rides the job's record through the
// hold ([LaunchRecord.CollectedAt]), and so does the harness's own terminal event.
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
			"want the first collection's %v and the harness's own event", ended.At, ended.Event, first)
	}
}
