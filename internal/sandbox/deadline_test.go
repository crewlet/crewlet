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
// read the job is over, and its own record says at most that a request ended
// it ([Result.JobEnd]), which names no cause. It is still the run its cap
// ended — never a success, its resume and its record saying the wall-clock cap
// ended it, and never parked on the question it asked on its way out —
// wherever its collection SAW it working past the cap: its poll answered that
// it ran; or, where that poll could not say, its script heard the request to
// end, or its own record says a request ended it. And a job that finished by
// itself before its cap's collection read it is no run the cap ended: not
// asked, not swept, and collected as any finished job is, whether or not the
// poll could say so.
//
// Mutation: read ENDED off whether the job is over once its end is done, and a
// run its cap stopped whose end left no record reads as one that finished;
// read it off the poll alone, and one whose poll could not say and whose
// script heard the request is said only to have been ended on request and is
// parked on its question; drop the job's own record of the request, and so is
// one whose request was heard and whose answer was lost; mark every collection
// whose poll could not say ended, or every collection past the cap, and a job
// that finished by itself is said cut off; end a job its poll found over, and
// it is swept.
func TestACappedRunStillSaysItWasCutOffAtItsCap(t *testing.T) {
	t.Parallel()
	recorded := Result{Text: "halfway through", JobEnd: JobEndAsked, Error: "the run was ended on request " +
		"before it finished: its harness was told to stop and exited 143"}
	unrecorded := Result{Text: "halfway through", Error: "the harness exited with status 143"}
	for _, tc := range []struct {
		name string
		// pollFails is a poll that cannot say whether the job still works.
		pollFails bool
		// finished is a job that finished by itself before its collection.
		finished bool
		// answer is how the request to end the job is answered: "" by a
		// script that hears it and ends the job; "lost" by one that heard it
		// and ended the job while its answer never came back; "gone" by a
		// box that finds the pid the script recorded no process of the job,
		// as a finished job's is.
		answer string
		// endsAs is what the job reports once a request ended it.
		endsAs Result
		// cutOff is a run its cap cut off, and asks the requests to end its job.
		cutOff bool
		asks   int
	}{
		{name: "its poll said it still worked", endsAs: recorded, cutOff: true, asks: 1},
		{name: "its poll said it still worked, and its end left no record", endsAs: unrecorded,
			cutOff: true, asks: 1},
		{name: "its poll could not say, and its script heard the request", pollFails: true,
			endsAs: unrecorded, cutOff: true, asks: 1},
		{name: "its poll could not say, and the answer to its request was lost", pollFails: true,
			answer: "lost", endsAs: recorded, cutOff: true, asks: 1},
		{name: "it finished before its collection", finished: true},
		{name: "its poll could not say, and it had finished by itself", pollFails: true, finished: true,
			answer: "gone", asks: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			launched := rig.launchUntil("t1", rig.now.Add(-time.Minute))
			rig.coordinator.countRun("swe", StatusRunning)
			box := rig.provider.Box(launched.SandboxID)
			if tc.cutOff {
				rig.ask("t1", Ask{Question: "which branch?", To: AskToRequester, Decision: choosing("which branch?"),
					Branch: "wip/t1"})
				// WHAT A JOB ITS END STOPPED REPORTS, which the collection
				// opens the run's failure with.
				rig.runner.EndsAs(tc.endsAs)
			}
			if tc.pollFails {
				rig.runner.PollErr = errors.New("the box's exec did not answer")
			}
			if tc.finished {
				rig.runner.Finish(Result{Success: true, Text: "checked the monitors"})
			}
			switch tc.answer {
			case "lost":
				rig.runner.EndFunc = func(_ context.Context, in Sandbox, _ RunHandle) (EndAsk, error) {
					rig.runner.EndJobIn(in)
					return "", errors.New("the box's exec did not answer")
				}
			case "gone":
				rig.runner.EndFunc = func(context.Context, Sandbox, RunHandle) (EndAsk, error) {
					return EndGone, nil
				}
			}

			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
			if asks := rig.runner.Asks(); asks != tc.asks {
				t.Fatalf("the job was asked to end %d times, want %d: asked wherever its poll did not find "+
					"it over", asks, tc.asks)
			}
			if tc.asks == 0 && len(box.JobCalls()) != 0 {
				t.Fatalf("the job that finished by itself saw %q; want it collected as any finished job is, "+
					"nothing of it asked or swept", box.JobCalls())
			}
			calls := rig.resumer.calls()
			if len(calls) != 1 {
				t.Fatalf("resumed %d times, want the turn back with what its run did; asked %+v", len(calls),
					rig.questions())
			}
			said := strings.Contains(calls[0].Answer, "wall-clock cap")
			if calls[0].Success == tc.cutOff || said != tc.cutOff {
				t.Fatalf("the resume reads success %v and %q; want a run cut off at its cap: %v",
					calls[0].Success, calls[0].Answer, tc.cutOff)
			}
			recs := rig.collected()
			if len(recs) != 1 || recs[0].Failed != tc.cutOff ||
				strings.Contains(recs[0].Error, "wall-clock cap") != tc.cutOff {
				t.Fatalf("the run's record is %+v; want it failed at its cap: %v", recs, tc.cutOff)
			}
			if !tc.cutOff {
				return
			}
			if !strings.Contains(calls[0].Answer, "It had asked: which branch?") ||
				!strings.Contains(calls[0].Answer, tc.endsAs.Error) {
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

// A CAP'S VERDICT OUTLIVES A READ CUT SHORT AS IT ENDS THE JOB. A collection
// that sees its job still working past its turn's cap — its poll answered that
// it ran, or, where that poll could not say, its script heard the request to
// end — records so on the job's record before it ends the job
// ([LaunchRecord.EndedAtDeadline]). The read can be cut short as it ends the
// job — its node draining, the seat moving — and the kill that outlives it can
// take the job's script before the script records the request, leaving the job
// over with nothing in its box that says a request ended it. The claim goes
// back, and the collection made again reads the job's record: the run is still
// the one its cap cut off, never a success, and never parked on the question
// it asked on its way out.
//
// Mutation: record nothing on the job's record, or only what the poll saw, and
// the run collected again reads as one that ended by itself, and is parked on
// its question.
func TestACapsVerdictOutlivesAReadCutShortAsItEndsTheJob(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		pollFails bool
	}{
		{name: "its poll saw it working"},
		{name: "its poll could not say, and its script heard the request", pollFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			launched := rig.launchUntil("t1", rig.now.Add(-time.Minute))
			rig.coordinator.countRun("swe", StatusRunning)
			rig.ask("t1", Ask{Question: "which branch?", To: AskToRequester, Decision: choosing("which branch?"),
				Branch: "wip/t1"})
			if tc.pollFails {
				rig.runner.PollErr = errors.New("the box's exec did not answer")
			}
			// THE READ IS CUT SHORT AS THE JOB'S SCRIPT IS ASKED TO END IT — its
			// node draining — and the kill that outlives it takes the job
			// before the script records the request: the job over, and its box
			// holding nothing that says a request ended it.
			ctx, drain := context.WithCancel(t.Context())
			defer drain()
			rig.runner.EndFunc = func(context.Context, Sandbox, RunHandle) (EndAsk, error) {
				drain()
				return EndAsked, nil
			}
			box := rig.provider.Box(launched.SandboxID)
			box.KillJobFunc = func(context.Context, string) error {
				rig.runner.EndJobIn(box)
				return nil
			}
			rig.runner.EndsAs(Result{Text: "halfway through", Error: "the harness exited with status 143"})
			// A BOX READ UNDER THE CUT-SHORT READ'S CONTEXT FAILS, as every
			// real box's does.
			rig.runner.CollectFunc = func(read context.Context) error { return read.Err() }

			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(ctx, payload, ev); err == nil {
				t.Fatal("the collection cut short as it ended its job answered")
			}
			if !slices.Contains(box.JobCalls(), "kill cmd-1") || len(rig.resumer.calls()) != 0 {
				t.Fatalf("the premise: the read cut short left %q and resumed %d times; want its job killed "+
					"and its turn not resumed", box.JobCalls(), len(rig.resumer.calls()))
			}

			// THE COLLECTION MADE AGAIN, on the claim the cut-short one handed back.
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted, made again: %v", err)
			}
			if asked := rig.questions(); len(asked) != 0 {
				t.Fatalf("announced %+v; a run its cap cut off asks nobody, however often it is collected", asked)
			}
			calls := rig.resumer.calls()
			if len(calls) != 1 {
				t.Fatalf("resumed %d times, want the turn back once with what its run did", len(calls))
			}
			if calls[0].Success || !strings.Contains(calls[0].Answer, "wall-clock cap") || calls[0].Run.Question != "" {
				t.Fatalf("the resume reads success %v and %q, carrying the question %q; want a run its cap cut "+
					"off, asking nobody", calls[0].Success, calls[0].Answer, calls[0].Run.Question)
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
	rig.ask("t1", Ask{Question: "which branch?", To: AskToRequester, Decision: choosing("which branch?"), Branch: "wip/t1"})

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
