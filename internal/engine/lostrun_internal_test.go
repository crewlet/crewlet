package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RUN LOST WITH ITS HOST CARRIES ITS ROUND ON IN A FRESH RUN: once the host's
// waiter announces the loss, the seat's holder claims it, reads the job's spend
// off its box and resumes the turn, relaunching the same round as a new job in
// the lost one's box, in a session of its own, told it was cut off. That job's
// collection ends the turn.
//
// Mutation: hand the runner no [sandbox.ResumeRequest.Lost], and the turn
// collects the truncated job as the round's result; no second job launches.
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
	runner := rig.runners[first.Harness]

	// THE ENGINE'S RESTART TOOK THE JOB, and its host's waiter said so.
	runner.Lose(sandbox.Result{Text: "half way through the fix", InputTokens: 300, OutputTokens: 30})
	pending := e.sandbox.Load().pending
	if marked, err := pending.MarkLost(t.Context(), "run-lost", first.LaunchID,
		sandbox.JobLoss{Cause: sandbox.LossEngineRestarted, Node: first.LaunchNode}); err != nil || !marked {
		t.Fatalf("MarkLost = %v, %v", marked, err)
	}
	completion := types.ExecutorFinished{
		Agent: first.AgentID, AgentHandle: first.AgentHandle, RoleName: first.Role,
		TurnID: first.TurnID, WorkKey: first.WorkKey, LaunchID: first.LaunchID,
		SandboxID: first.SandboxID, Harness: first.Harness,
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

// A ROUND LOST PAST ITS LIMIT ENDS ITS TURN AT ONCE. A round whose jobs keep
// being lost is a run that takes its host down, so the resume that would carry
// it on again refuses (runner.ErrRoundLost), permanently, since every retry
// carries the same losses: the run is settled and its turn ended on that
// resume, whatever its jobs' calls showed, these having made none.
//
// Mutation: drop turn.ErrPermanent from the refusal (internal/agent/runner's
// lost.go), and the resume after the last loss is handed back rather than
// settled, leaving the run on the record.
func TestARoundLostPastItsLimitEndsItsTurnAtOnce(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	rig := rigRuns(t, e, modelPlayer(t, billingRun))
	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-lost-limit", RunID: "run-lost-limit", WorkSince: time.Now().UTC(),
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; want the turn parked on its executor's run", res, err)
	}
	pending := e.sandbox.Load().pending
	runner := rig.runners[rig.row(t, "run-lost-limit").Harness]
	for loss := 1; loss <= sandbox.MaxRoundLosses+1; loss++ {
		// THE ENGINE'S RESTART TOOK THE ROUND'S NEWEST JOB, and its host's
		// waiter said so.
		row := rig.row(t, "run-lost-limit")
		runner.Lose(sandbox.Result{Text: "half way through the fix", InputTokens: 300, OutputTokens: 30})
		if marked, err := pending.MarkLost(t.Context(), "run-lost-limit", row.LaunchID,
			sandbox.JobLoss{Cause: sandbox.LossEngineRestarted, Node: row.LaunchNode}); err != nil || !marked {
			t.Fatalf("loss %d: MarkLost = %v, %v", loss, marked, err)
		}
		completion := types.ExecutorFinished{
			Agent: row.AgentID, AgentHandle: row.AgentHandle, RoleName: row.Role,
			TurnID: row.TurnID, WorkKey: row.WorkKey, LaunchID: row.LaunchID,
			SandboxID: row.SandboxID, Harness: row.Harness,
		}
		ev := events.New(completion, events.TraceContext{TraceID: row.TraceID, ParentSpanID: row.SpanID})
		if err := e.sandbox.Load().coordinator.OnCompleted(t.Context(), completion, ev); err != nil {
			t.Fatalf("loss %d: claiming the lost job: %v", loss, err)
		}
	}
	if row, found, err := pending.Get(t.Context(), "run-lost-limit"); err != nil || found {
		t.Fatalf("the run is still on the record after its round was lost past its limit (%s, "+
			"ending %+v, %v); want it settled, its turn ended", row.Status, row.Ending, err)
	}
	if n := len(runner.Started()); n != sandbox.MaxRoundLosses+1 {
		t.Errorf("%d jobs were started, want the first and one carrying the round on after each of "+
			"the %d losses a round is carried on after", n, sandbox.MaxRoundLosses)
	}
}
