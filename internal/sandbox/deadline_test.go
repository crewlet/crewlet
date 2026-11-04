package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// launchUntil is [waiterRig.launch] for a turn whose wall-clock cap ends at
// deadline: the launch writes the turn's deadline onto the row it opens.
func (r *waiterRig) launchUntil(turnID string, deadline time.Time) PendingRun {
	r.t.Helper()
	ctx := r.t.Context()
	box, err := r.provider.Create(ctx, Spec{})
	if err != nil {
		r.t.Fatalf("Create: %v", err)
	}
	if _, err := r.pending.BeginLaunch(ctx, PendingRun{
		TurnID: turnID, AgentHandle: "swe", AgentID: sweID, Role: "SWE",
		Harness: "claude-code", CreatedAt: r.now, Deadline: deadline,
	}, rigLease); err != nil {
		r.t.Fatalf("BeginLaunch: %v", err)
	}
	if err := r.pending.AttachSandbox(ctx, turnID, BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", Harness: "claude-code",
	}, rigLease); err != nil {
		r.t.Fatalf("AttachSandbox: %v", err)
	}
	r.suspend(turnID)
	return r.get(turnID)
}

// A RUN THAT OUTLASTS ITS TURN'S CAP IS ENDED, AS IF IT HAD FINISHED. The turn's
// own segments refuse to start a round past its deadline, and a detached run is
// no round of theirs: a scheduled turn with a half-hour cap had runs that went
// on for hours, bounded by their turn cap alone. The poll fires the completion
// of a run still working once the deadline has passed — and not a moment
// before, nor for a turn with no cap.
func TestARunPastItsTurnsDeadlineIsEnded(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launchUntil("capped", rig.now.Add(time.Minute))
	rig.launch("uncapped")

	if fired := rig.tick(); fired != 0 {
		t.Fatalf("fired %d completions before any deadline passed", fired)
	}
	rig.now = rig.now.Add(2 * time.Minute)
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("fired %d completions, want the one run past its turn's deadline", fired)
	}
}

// AND ITS COLLECTION SAYS SO, AND KEEPS EVERYTHING IT DID. The job was working
// when the poll ended it, so what is collected is what it had done by then —
// never a success, since it did not get to say it was done — and the resumed
// turn judges it on every call it made, those that landed after the claim read
// the row included — and the identities of those its log dropped from its
// middle with them, which the resumed turn's call log is seeded by.
//
// Mutation: keep no identity of a call the log drops from its middle
// ([BridgeLog.appendCall]), and the resumed turn counts two calls its run made
// as none.
func TestARunEndedAtItsDeadlineIsCollectedWithWhatItDid(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launchUntil("t1", rig.now.Add(-time.Minute))
	rig.coordinator.countRun("swe", StatusRunning)
	// The job is still working: the fake reports a result only for what it
	// streamed so far, and nothing finished it.
	rig.runner.CollectFunc = func(ctx context.Context) error {
		// A call the run made while its completion was being claimed, and
		// then enough more that its log dropped two from its middle.
		row := rig.getIn(ctx, "t1")
		for i := range MaxBridgeCalls + 2 {
			call := BridgeCall{Name: "read_file", At: rig.now, Digest: fmt.Sprintf("%064x", i)}
			if i == 0 {
				call = BridgeCall{Name: "comment_on_work_item", At: rig.now}
			}
			if _, err := rig.pending.AppendBridgeCall(ctx, "t1", BridgeAppend{
				Launch: row.LaunchID, Call: call,
			}); err != nil {
				t.Errorf("AppendBridgeCall: %v", err)
				return nil
			}
		}
		return nil
	}

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times, want the turn back with what its run did", len(calls))
	}
	if calls[0].Success || !strings.Contains(calls[0].Answer, "wall-clock cap") {
		t.Errorf("the resume reads success %v and %q, want the run said to be cut off at its cap",
			calls[0].Success, calls[0].Answer)
	}
	if !slices.ContainsFunc(calls[0].Bridged.Calls, func(c BridgeCall) bool {
		return c.Name == "comment_on_work_item"
	}) {
		t.Errorf("the resume judges the run on %+v, missing the call it made as it was ended",
			calls[0].Bridged.Calls)
	}
	if got := calls[0].Bridged; got.Elided != 2 || !got.ElisionKnown() {
		t.Errorf("the resume reads %d calls dropped from the log's middle, accounted for by %+v; "+
			"want the two with their identities", got.Elided, got.ElidedCalls)
	}
}

// A CAPPED RUN STILL SAYS IT WAS CUT OFF AT ITS CAP. Its collection ends a job
// its turn's cap passed while it still worked, so by the time its result is
// read the job is over, and its own record says only that a request ended it
// ([Result.JobEnd]), which names no cause. It is still the run its cap ended:
// never a success, its resume and its record saying the wall-clock cap ended
// it, and never parked on the question it asked on its way out. That holds
// where the poll before the end could not say whether the job still worked,
// since its record of the request it heard says it did. And a job that
// finished by itself before its cap's collection read it is no run the cap
// ended, and is not ended again.
//
// Mutation: read ENDED off whether the job is over once its end is done, and a
// run its cap stopped reads as one that finished; read it off the poll alone,
// and a run whose poll could not say is said only to have been ended on
// request and is parked on its question; mark every collection past the cap
// ended, and a job that finished before its read is said cut off.
func TestACappedRunStillSaysItWasCutOffAtItsCap(t *testing.T) {
	t.Parallel()
	const endedOnRequest = "the run was ended on request before it finished: its harness was told to " +
		"stop and exited 143"
	for _, tc := range []struct {
		name                string
		pollFails, finished bool
	}{
		{name: "its poll said it still worked"},
		{name: "its poll could not say", pollFails: true},
		{name: "it finished before its collection", finished: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			rig.launchUntil("t1", rig.now.Add(-time.Minute))
			rig.coordinator.countRun("swe", StatusRunning)
			stoppedAtCap := !tc.finished
			if stoppedAtCap {
				rig.ask("t1", Ask{Question: "which branch?", To: AskToRequester, Branch: "wip/t1"})
			}
			if tc.pollFails {
				rig.runner.PollErr = errors.New("the box's exec did not answer")
			}
			if tc.finished {
				rig.runner.Finish(Result{Success: true, Text: "checked the monitors"})
			}
			// WHAT A JOB ITS END STOPPED REPORTS: its script's record of the
			// request, which the collection opens the run's failure with.
			rig.runner.EndsAs(Result{Text: "halfway through", JobEnd: JobEndAsked, Error: endedOnRequest})

			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
			wantAsks := 0
			if stoppedAtCap {
				wantAsks = 1
			}
			if asks := rig.runner.Asks(); asks != wantAsks {
				t.Fatalf("the job was asked to end %d times, want %d: ended only where its cap cut it off",
					asks, wantAsks)
			}
			calls := rig.resumer.calls()
			if len(calls) != 1 {
				t.Fatalf("resumed %d times, want the turn back with what its run did; asked %+v", len(calls),
					rig.questions())
			}
			said := strings.Contains(calls[0].Answer, "wall-clock cap")
			if calls[0].Success == stoppedAtCap || said != stoppedAtCap {
				t.Fatalf("the resume reads success %v and %q; want a run cut off at its cap: %v",
					calls[0].Success, calls[0].Answer, stoppedAtCap)
			}
			recs := rig.collected()
			if len(recs) != 1 || recs[0].Failed != stoppedAtCap ||
				strings.Contains(recs[0].Error, "wall-clock cap") != stoppedAtCap {
				t.Fatalf("the run's record is %+v; want it failed at its cap: %v", recs, stoppedAtCap)
			}
			if !stoppedAtCap {
				return
			}
			if !strings.Contains(calls[0].Answer, "It had asked: which branch?") ||
				!strings.Contains(calls[0].Answer, endedOnRequest) {
				t.Errorf("the resume reads %q; want the cap's account with the question it had asked and how "+
					"its job ended", calls[0].Answer)
			}
			if asked := rig.questions(); len(asked) != 0 || calls[0].Run.Question != "" {
				t.Errorf("announced %+v and resumed carrying %q; a run its cap ended asks nobody", asked,
					calls[0].Run.Question)
			}
		})
	}
}

// AND IT IS NOT PARKED ON A QUESTION IT ASKED ON ITS WAY. A run the clock is
// stopping is not one a person's answer could continue, so a question it
// recorded before its cap ended it is collected with the rest of what it did:
// the turn is resumed, judging a run cut off at its cap, and nobody is asked.
// The question is still SAID, in the note that ends it: the turn and the
// seat's next turn read that the run was waiting on a person, and what for.
//
// Mutation: park on the claimed row's question whether or not the run was
// ended at its cap, and the run waits on an answer nothing will continue;
// leave the question out of the note, and it is lost without a trace.
func TestARunEndedAtItsDeadlineIsNotParkedOnItsQuestion(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launchUntil("t1", rig.now.Add(-time.Minute))
	rig.coordinator.countRun("swe", StatusRunning)
	rig.ask("t1", Ask{Question: "which branch?", To: AskToRequester, Branch: "wip/t1"})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if asked := rig.questions(); len(asked) != 0 {
		t.Fatalf("announced %+v; a run ended at its cap asks nobody", asked)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times, want the turn back with what its run did", len(calls))
	}
	if calls[0].Success || !strings.Contains(calls[0].Answer, "wall-clock cap") ||
		!strings.Contains(calls[0].Answer, "It had asked: which branch?") {
		t.Errorf("the resume reads success %v and %q, want the run said to be cut off at its "+
			"cap with the question it had asked", calls[0].Success, calls[0].Answer)
	}
	if calls[0].Run.Question != "" || calls[0].Run.Branch != "" {
		t.Errorf("the resume carries the run's question %q and branch %q; want neither, as nothing is "+
			"waiting on them", calls[0].Run.Question, calls[0].Run.Branch)
	}
	recs := rig.collected()
	if len(recs) != 1 || strings.Contains(recs[0].Notes, "stopped to ask") || !recs[0].Failed ||
		!strings.Contains(recs[0].Error, "It had asked: which branch?") {
		t.Errorf("the run's record is %+v; want it failed at its cap, not stopped to ask, "+
			"naming the question it had asked", recs)
	}
	if run, found, err := rig.pending.Get(t.Context(), "t1"); err != nil ||
		(found && run.Status == StatusAwaiting) {
		t.Fatalf("the row is %q (found %v, %v); want the run not parked", run.Status, found, err)
	}
}
