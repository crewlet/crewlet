package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// openingAhead is a run store that lets a case act as a launch begins, before
// the write that opens the run's record and its log to the job: inside the
// window between the read a relaunch's briefs were built from and its opening.
type openingAhead struct {
	sandbox.PendingStore
	mu     sync.Mutex
	before func(turnID string)
}

// ahead runs hook, once, as the next launch begins.
func (s *openingAhead) ahead(hook func(turnID string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.before = hook
}

func (s *openingAhead) BeginLaunch(ctx context.Context, run sandbox.PendingRun,
	fence sandbox.Fence) (sandbox.PendingRun, error) {
	s.mu.Lock()
	hook := s.before
	s.before = nil
	s.mu.Unlock()
	if hook != nil {
		hook(run.TurnID)
	}
	return s.PendingStore.BeginLaunch(ctx, run, fence)
}

// A RESUME'S READ REACHES ITS RELAUNCH'S OPENING, through the coordinator, the
// engine's resumer and the runner. A lost job's round is carried on by a
// relaunch whose briefs are built from the read the coordinator makes of the
// round's log as it resumes the turn: the coordinator counts the calls that
// read saw (sandbox.ResumeRequest.BridgedRead), the engine's resumer hands the
// count to the turn's runner, the runner hands it to its launch
// (runner.Continuation.BriefRead), and the launch to the store, whose opening
// marks late every call of the round the log took after it. So a call of the
// lost job that lands while the relaunch is readied — after the read and
// before the opening, as that job's own — is listed late, and the call the
// read saw is not.
//
// Mutation: drop the count anywhere between the coordinator's read and the
// opening — the coordinator's count, or the resumer's copy of it — and the call
// the read saw is marked late with the rest.
func TestAResumesReadReachesItsRelaunchsOpening(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const turnID = "run-read-to-opening"
	e, _ := spendingEngine(t)
	store := &openingAhead{PendingStore: sandbox.NewCoordStore(e.backends.Fleet)}
	rig := rigRunsOn(t, e, store, modelPlayer(t, billingRun))
	res, err := e.runTurn(ctx, Request{
		Handle: "swe", WorkKey: "wk-read", RunID: turnID, WorkSince: time.Now().UTC(),
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; want the turn parked on its executor's run", res, err)
	}
	first := rig.row(t, turnID)
	// call records a call of the first job on the round's log, as that job's
	// session's ledger does.
	call := func(name string, seq int) {
		recorded, err := store.PendingStore.AppendBridgeCall(t.Context(), turnID, sandbox.BridgeAppend{
			Launch: first.LaunchID, Round: first.Round,
			Call: sandbox.BridgeCall{Name: name, Seq: seq, StartedAt: time.Now().UTC()},
		})
		if err != nil || !recorded {
			t.Errorf("the first job's %s was not recorded as its own (%v, %v)", name, recorded, err)
		}
	}
	// THE CALL THE RESUME'S READ SEES.
	call("read_page", 1)
	// THE CALL LANDING WHILE THE RELAUNCH IS READIED: as its launch begins,
	// after the resume's read of the log and before its opening moves the log
	// on, so as the lost job's own.
	store.ahead(func(string) { call("post_message", 2) })

	// THE ENGINE'S RESTART TOOK THE JOB, and its host's waiter said so.
	rig.runners[first.Harness].Lose(sandbox.Result{Text: "half way through the fix", InputTokens: 300,
		OutputTokens: 30})
	if marked, err := store.MarkLost(ctx, turnID, first.LaunchID,
		sandbox.JobLoss{Cause: sandbox.LossEngineRestarted, Node: first.LaunchNode}); err != nil || !marked {
		t.Fatalf("MarkLost = %v, %v", marked, err)
	}
	completion := types.ExecutorFinished{
		Agent: first.AgentID, AgentHandle: first.AgentHandle, RoleName: first.Role,
		TurnID: first.TurnID, WorkKey: first.WorkKey, LaunchID: first.LaunchID,
		SandboxID: first.SandboxID, Harness: first.Harness,
	}
	ev := events.New(completion, events.TraceContext{TraceID: first.TraceID, ParentSpanID: first.SpanID})
	if err := e.sandbox.Load().coordinator.OnCompleted(ctx, completion, ev); err != nil {
		t.Fatalf("claiming the lost job: %v", err)
	}
	if next := rig.row(t, turnID); next.LaunchID == first.LaunchID {
		t.Fatalf("the lost job's round was not carried on: the run still holds job %s", first.LaunchID)
	}

	listed := map[string][]sandbox.BridgeCall{}
	for _, c := range rig.calls(t, turnID).Calls {
		listed[c.Name] = append(listed[c.Name], c)
	}
	if seen := listed["read_page"]; len(seen) != 1 || seen[0].Late {
		t.Errorf("the call the resume's read saw is listed %+v, want it once and not late", seen)
	}
	if landed := listed["post_message"]; len(landed) != 1 || !landed[0].Late {
		t.Errorf("the call that landed while the relaunch was readied is listed %+v, want it once and late",
			landed)
	}
}
