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
