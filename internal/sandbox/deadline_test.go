package sandbox

import (
	"context"
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
		CodingAgent: "claude-code", CreatedAt: r.now, Deadline: deadline,
	}, rigLease); err != nil {
		r.t.Fatalf("BeginLaunch: %v", err)
	}
	if err := r.pending.AttachSandbox(ctx, turnID, BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", CodingAgent: "claude-code",
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
// the row included.
func TestARunEndedAtItsDeadlineIsCollectedWithWhatItDid(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launchUntil("t1", rig.now.Add(-time.Minute))
	rig.coordinator.countRun("swe", StatusRunning)
	// The job is still working: the fake reports a result only for what it
	// streamed so far, and nothing finished it.
	rig.runner.CollectFunc = func(ctx context.Context) error {
		// A call the run made while its completion was being claimed.
		row := rig.getIn(ctx, "t1")
		if _, err := rig.pending.AppendBridgeCall(ctx, "t1", BridgeAppend{
			Launch: row.LaunchID, Call: BridgeCall{Name: "comment_on_work_item", At: rig.now},
		}); err != nil {
			t.Errorf("AppendBridgeCall: %v", err)
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
	if !slices.ContainsFunc(calls[0].Run.BridgeCalls, func(c BridgeCall) bool {
		return c.Name == "comment_on_work_item"
	}) {
		t.Errorf("the resume judges the run on %+v, missing the call it made as it was ended",
			calls[0].Run.BridgeCalls)
	}
}

// AND IT IS NOT PARKED ON A QUESTION IT ASKED ON ITS WAY. A run the clock is
// stopping is not one a person's answer could continue, so a question it
// recorded before its cap ended it is collected with the rest of what it did:
// the turn is resumed, judging a run cut off at its cap, and nobody is asked.
//
// Mutation: park on the claimed row's question whether or not the run was
// ended at its cap, and the run waits on an answer nothing will continue.
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
	if calls[0].Success || !strings.Contains(calls[0].Answer, "wall-clock cap") {
		t.Errorf("the resume reads success %v and %q, want the run said to be cut off at its cap",
			calls[0].Success, calls[0].Answer)
	}
	if calls[0].Run.Question != "" || calls[0].Run.Branch != "" {
		t.Errorf("the resume carries the run's question %q and branch %q; want neither, as nothing is "+
			"waiting on them", calls[0].Run.Question, calls[0].Run.Branch)
	}
	recs := rig.phases()
	if len(recs) != 1 || strings.Contains(recs[0].Notes, "stopped to ask") || !recs[0].Failed {
		t.Errorf("the run's record is %+v; want it failed at its cap, not stopped to ask", recs)
	}
	if run, found, err := rig.pending.Get(t.Context(), "t1"); err != nil ||
		(found && run.Status == StatusAwaiting) {
		t.Fatalf("the row is %q (found %v, %v); want the run not parked", run.Status, found, err)
	}
}
