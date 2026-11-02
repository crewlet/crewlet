package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RUN LOST WITH ITS HOST CARRIES ITS ROUND ON IN A FRESH RUN, and the turn
// goes on to its end from there. The restart that replaced the engine killed the
// run's job mid-step; the host's waiter recorded the loss and announced it, and
// the seat's holder — this engine — claims it, reads what the job spent off its
// box and resumes the turn, which relaunches the same round: a new job in the
// lost one's box, in a session of its own, told it was cut off. That job's
// collection ends the turn as any run's does.
//
// Mutation: drop the loss from the resume ([sandbox.ResumeRequest.Lost] not
// handed to the runner), and the turn collects the truncated job as the
// round's result and ends there — no second job is ever launched.
func TestARunLostWithItsHostCarriesItsRoundOnInAFreshRun(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	rig := rigRuns(t, e, modelPlayer(t, billingRun))
	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-lost", RunID: "run-lost", WorkSince: time.Now().UTC(),
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; want the turn parked on its executor's run", res, err)
	}
	first := rig.row(t, "run-lost")
	runner := rig.runners[first.CodingAgent]

	// THE ENGINE'S RESTART TOOK THE JOB, and its host's waiter said so.
	runner.Lose(sandbox.Result{Text: "half way through the fix", InputTokens: 300, OutputTokens: 30})
	pending := e.sandbox.Load().pending
	if marked, err := pending.MarkLost(t.Context(), "run-lost", first.LaunchID,
		sandbox.JobLoss{Cause: sandbox.LossEngineRestarted, Node: first.LaunchNode}); err != nil || !marked {
		t.Fatalf("MarkLost = %v, %v", marked, err)
	}
	completion := types.SandboxRunCompleted{
		Agent: first.AgentID, AgentHandle: first.AgentHandle, RoleName: first.Role,
		TurnID: first.TurnID, WorkKey: first.WorkKey, LaunchID: first.LaunchID,
		SandboxID: first.SandboxID, CodingAgent: first.CodingAgent,
	}
	ev := events.New(completion, events.TraceContext{TraceID: first.TraceID, ParentSpanID: first.SpanID})
	if err := e.sandbox.Load().coordinator.OnCompleted(t.Context(), completion, ev); err != nil {
		t.Fatalf("claiming the lost job: %v", err)
	}

	second := rig.row(t, "run-lost")
	if second.LaunchID == first.LaunchID || second.Status != sandbox.StatusRunning || second.Lost != nil {
		t.Fatalf("after the loss the run holds job %s (%s, lost %+v); want a fresh job of the same "+
			"round running", second.LaunchID, second.Status, second.Lost)
	}
	if second.RoundLosses != 1 {
		t.Errorf("the carried-on round counts %d lost jobs, want 1", second.RoundLosses)
	}
	started := runner.Started()
	if len(started) != 2 {
		t.Fatalf("%d jobs were started, want the lost one and the one carrying its round on", len(started))
	}
	if again := started[1]; again.Resume != "" || !strings.Contains(again.Brief, "was cut off") {
		t.Fatalf("the second job continues session %q with a brief that does not say the run "+
			"before it was cut off:\n%s", again.Resume, again.Brief)
	}

	if err := rig.collect(t, "run-lost"); err != nil {
		t.Fatalf("collecting the job that carried the round on: %v", err)
	}
	if row, found, err := pending.Get(t.Context(), "run-lost"); err != nil || found {
		t.Fatalf("the run is still on the record after its carried-on round was collected (%s, %v)",
			row.Status, err)
	}
}
