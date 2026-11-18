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

// launchUntil is [waiterRig.launch] with the turn's wall-clock cap ending at
// deadline, written onto the row it opens.
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

// A RUN THAT OUTLASTS ITS TURN'S CAP IS ENDED, AS IF IT HAD FINISHED. A
// detached run is no round of the turn's segments, so their deadline check
// never stops it; the poll fires its completion once the deadline has passed,
// not before, and never for a turn with no cap.
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

// AND ITS COLLECTION SAYS SO, AND KEEPS EVERYTHING IT DID: never a success,
// since it never said it was done, and the resumed turn judges it on every
// call, those landing after the claim read the row included, plus the
// identities of calls its log dropped from the middle, which seed the resumed
// turn's call log.
//
// Mutation: keep no identity of a call the log drops from its middle
// ([BridgeLog.appendCall]), and the resumed turn counts two calls its run made
// as none.
func TestARunEndedAtItsDeadlineIsCollectedWithWhatItDid(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launchUntil("t1", rig.now.Add(-time.Minute))
	rig.coordinator.countRun("swe", StatusRunning)
	// The job is still working: nothing finished it.
	rig.runner.CollectFunc = func(ctx context.Context) error {
		// Calls made during the claim, enough that the log drops two from its middle.
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

// A CAPPED RUN STILL SAYS IT WAS CUT OFF AT ITS CAP. By the time its result is
// read the job is over, its record naming at most a request ([Result.JobEnd]),
// no cause. Wherever the collection SAW it working past the cap (its poll said
// so, or else its script heard the request or its record names one), it fails
// citing the wall-clock cap and is never parked on its question. A job that
// finished before the collection read it is collected as finished: not asked,
// not swept, whatever the poll could say.
//
// Mutation: read ENDED off the job being over, off the poll alone, or without
// the job's own record of the request; mark every unsure poll or every
// collection past the cap ended; or end a job its poll found over; and a case
// goes red.
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
		// answer is how the request to end the job is answered: "" heard and
		// ended; "lost" heard and ended, the answer never back; "gone" the
		// recorded pid no longer the job's, as after it finished.
		answer string
		// endsAs is what the job reports once a request ended it.
		endsAs Result
		// cutOff is a run its cap cut off; asks counts requests to end its job.
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
				// What the stopped job reports, which opens the run's failure.
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
// that sees its job working past the cap (its poll said so, or else its
// script heard the request) records so ([LaunchRecord.EndedAtDeadline]) before
// ending it, because a read cut short
// there (node draining, seat moving) can let the kill take the script before
// it records the request, leaving nothing in the box. The retried collection
// reads the record: still cut off at its cap, never a success, never parked on
// its question.
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
			// The read is cut short as the script is asked to end the job, and
			// the kill takes the job before the script records the request.
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
			// A box read under the cancelled context fails, as a real box's does.
			rig.runner.CollectFunc = func(read context.Context) error { return read.Err() }

			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(ctx, payload, ev); err == nil {
				t.Fatal("the collection cut short as it ended its job answered")
			}
			if !slices.Contains(box.JobCalls(), "kill cmd-1") || len(rig.resumer.calls()) != 0 {
				t.Fatalf("the premise: the read cut short left %q and resumed %d times; want its job killed "+
					"and its turn not resumed", box.JobCalls(), len(rig.resumer.calls()))
			}

			// The collection again, on the claim the cut-short one handed back.
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

// AND IT IS NOT PARKED ON A QUESTION IT ASKED ON ITS WAY: no answer could
// continue a run the clock stopped, so the turn resumes judging it cut off and
// nobody is asked. The question is still said in the closing note, so the turn
// and the seat's next turn see what the run was waiting on.
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
