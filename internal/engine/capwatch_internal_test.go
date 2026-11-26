package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CAPPED RUN IS WATCHED FROM THE MOMENT ITS TURN PARKS, by this node's
// waiter, before any pass of the waiter lists it: the park that opens the run
// to the poll hands it to the turn cap's watch ([sandbox.Waiter.Watch]), so
// the watch's next tick reads its count and ends it past its cap. Here the
// waiter's pass is an hour away and never runs.
//
// Mutation: drop the park's hand-over to the watch, and the tick reads no
// count and ends nothing.
func TestACappedRunIsWatchedFromTheMomentItsTurnParks(t *testing.T) {
	t.Parallel()
	e, _ := starting(t, refusingModels(t))
	rig := rigRuns(t, e, nil)
	company := e.Company()
	seat := company.Org.AgentSeatByHandle("swe")
	r, err := company.RunnerFor(t.Context(), "swe", nil, RunnerInput{
		Reply: turn.NoReply(), Launcher: launcherStub{}, Sessions: e.sessions,
		Turn: runner.Turn{RunID: "run-code", Context: &turnctx.Turn{RunID: "run-code", Seat: seat,
			Org: company.Org}},
	})
	if err != nil {
		t.Fatalf("RunnerFor: %v", err)
	}
	res, err := turn.Run(t.Context(), r, company.TurnSettings(time.Time{}),
		turn.Input{RunID: "run-code", Reply: turn.NoReply()})
	if err != nil || !res.Suspended {
		t.Fatalf("turn.Run = %+v, %v; the turn did not park", res, err)
	}
	rt := e.sandbox.Load()
	waiter, err := sandbox.NewWaiter(e.sandboxWaiterOptions(rt, time.Hour))
	if err != nil {
		t.Fatalf("NewWaiter: %v", err)
	}
	if !e.sandboxWaiter.CompareAndSwap(nil, waiter) {
		t.Fatal("the premise: the engine already runs a waiter of its own")
	}
	// THE ROW AND THE BOX THE LAUNCH WOULD HAVE OPENED, capped at 3, which the
	// stub opened none of.
	provider, err := rt.coordinator.Manager().Provider(sandbox.Direct)
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	box, err := provider.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := rt.pending.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "run-code", AgentHandle: "swe", Role: "SWE", Harness: "claude-code", LaunchNode: e.id,
		Launch: sandbox.LaunchRecord{TurnCap: 3},
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := rt.pending.AttachSandbox(t.Context(), "run-code", sandbox.BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", Harness: "claude-code",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	if resumable, err := e.persistSuspension(t.Context(), r, "run-code", turnTelemetry{
		written: &turnctx.Written{}, ask: "the ask",
	}, segmentCharge{}); err != nil || !resumable {
		t.Fatalf("persistSuspension = %v, %v; want the run recorded", resumable, err)
	}

	fake := rig.runners["claude-code"]
	fake.Show(sandbox.RequestCount{Requests: 4})
	if fired := waiter.WatchTick(t.Context()); fired != 1 || len(fake.Counted()) != 1 {
		t.Fatalf("the watch read %d counts and ended %d runs; want the run its turn just parked ended past "+
			"its cap", len(fake.Counted()), fired)
	}
}
