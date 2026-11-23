package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// A LATE CALL REACHES ITS ROUND'S LOG THROUGH ITS SESSION'S LEDGER, which is
// bound to its job and the round that job works in and names both on every
// append. So a call answered only after a relaunch carried the round on (the
// log opened to the next job meanwhile) is listed in the round's log, marked
// late, rather than dropped; the next job's resume hands the runner the call
// with its mark; and the next job's record is not charged for it.
//
// Mutation: append under the job alone, or bind the ledger to the job without
// its round, and the call is dropped; carry no mark to the runner, and the
// resume reads it as any call.
func TestALateCallReachesItsRoundsLogThroughItsSessionsLedger(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(memory.NewFleet())
	first, err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe"}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	l := &bridgeLedger{store: store, spend: &steppingMeter{}}
	l.bind(first.NamedJob(first.OpenedAfter, "", 0))

	next, err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe", Continues: true},
		sandbox.Fence{})
	if err != nil {
		t.Fatalf("the relaunch that carries the round on: %v", err)
	}
	if next.Round != first.Round {
		t.Fatalf("the relaunch works in round %q, want the one it carried on, %q", next.Round, first.Round)
	}
	started := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	if err := l.Record(ctx, "run-1", mcpbridge.Call{Call: tools.Call{Name: "delegate"}, Seq: 1,
		StartedAt: started}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	logged, err := store.BridgeLog(ctx, next)
	if err != nil {
		t.Fatalf("BridgeLog: %v", err)
	}
	if len(logged.Calls) != 1 || logged.Calls[0].Name != "delegate" || !logged.Calls[0].Late {
		t.Fatalf("the round's log lists %+v, want the old session's late call, marked", logged.Calls)
	}
	// AND A RESUME OF THE NEXT JOB HANDS THE RUNNER THE CALL MARKED, which no
	// save point of the round then holds (ledger.Call.Late).
	if resumed := runnerResume(resumeInput{Run: next, Bridged: logged}, nil, nil, nil); len(resumed.Bridged) != 1 ||
		!resumed.Bridged[0].Late {
		t.Errorf("the next job's resume hands the runner %+v, want the late call marked", resumed.Bridged)
	}
	row, found, err := store.Get(ctx, "run-1")
	if err != nil || !found {
		t.Fatalf("Get: found %v, %v", found, err)
	}
	if row.Launch.Bridged != (sandbox.EngineSpend{}) {
		t.Errorf("the next job's record holds a bridged spend of %+v, want none: the late call's is no job's",
			row.Launch.Bridged)
	}
}

// appendCounter is a pending store that counts the bridged appends asked of
// the store it wraps.
type appendCounter struct {
	sandbox.PendingStore
	asked atomic.Int32
}

func (s *appendCounter) AppendBridgeCall(ctx context.Context, turnID string, a sandbox.BridgeAppend) (bool, error) {
	s.asked.Add(1)
	return s.PendingStore.AppendBridgeCall(ctx, turnID, a)
}

// A RECORD OF AN EARLIER ROUND REACHES THE NEXT ROUND'S LOG THROUGH ITS
// SESSION'S LEDGER. A call nobody waited for, still running as its round was
// collected, returns once a send-back opened the run's log to a new round: the
// store lists it late in that round, with no place, and answers it recorded
// beside sandbox.ErrJobReplaced, so the ledger asks once, answers no error,
// and says nothing at warning, the call being on the run's log.
//
// Mutation: answer a record of another round as taken for nothing, and the
// call is on no log and named at warning.
func TestARecordOfAnEarlierRoundReachesTheNextRoundsLogThroughItsSessionsLedger(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := &appendCounter{PendingStore: sandbox.NewCoordStore(memory.NewFleet())}
	first, err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe"}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	l, logs := readingLedger(store)
	l.bind(first.NamedJob(first.OpenedAfter, "", 0))
	next, err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe"}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("the send-back's launch: %v", err)
	}
	if next.Round == first.Round {
		t.Fatalf("the send-back works in round %q, the one before it: the case exercises nothing", next.Round)
	}
	started := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	if err := l.Record(ctx, "run-1", mcpbridge.Call{Call: tools.Call{Name: "delegate"}, Seq: 4,
		StartedAt: started, Abandoned: true}); err != nil {
		t.Fatalf("the record of the earlier round answered %v, want no error", err)
	}
	if n := store.asked.Load(); n != 1 {
		t.Errorf("the record was asked of the store %d times, want once", n)
	}
	logged, err := store.BridgeLog(ctx, next)
	if err != nil {
		t.Fatalf("BridgeLog: %v", err)
	}
	if len(logged.Calls) != 1 || logged.Calls[0].Name != "delegate" || !logged.Calls[0].Late ||
		logged.Calls[0].Seq != 0 {
		t.Errorf("the next round's log lists %+v, want the earlier round's call, late with no place", logged.Calls)
	}
	if line := logLine(t, logs, "bridge_abandoned_call_after_its_job"); line != nil {
		t.Errorf("the ledger named the call at warning, though the next round's log took it: %v", line)
	}
}

// A RESUME HANDS THE RUNNER HOW MANY LATE CALLS ITS LOG DROPPED. Past
// [sandbox.MaxBridgeCalls] the log drops calls from its middle, keeping only
// their identity and not the late mark, so it counts each late one it drops
// (sandbox.BridgeLog.LateElided) and a resume hands the runner the count
// (runner.Resume.LateElided), told to a relaunch continuing a kept session
// whatever its save point.
//
// Mutation: hand the runner none of the count, and a dropped late call is told
// to no continuation whose save point is past its number.
func TestAResumeHandsTheRunnerHowManyLateCallsItsLogDropped(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(memory.NewFleet())
	first, err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe"}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	next, err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe", Continues: true},
		sandbox.Fence{})
	if err != nil || next.Round != first.Round {
		t.Fatalf("the relaunch that carries the round on = round %q, %v; want %q", next.Round, err, first.Round)
	}
	// EVERY CALL LATE, so the log's middle holds no ordinary call to give up
	// first: past its bound, each call it drops is a late one.
	const dropped = 3
	for n := 1; n <= sandbox.MaxBridgeCalls+dropped; n++ {
		recorded, err := store.AppendBridgeCall(ctx, "run-1", sandbox.BridgeAppend{Launch: first.LaunchID,
			Round: first.Round, Call: sandbox.BridgeCall{Name: fmt.Sprintf("read_%03d", n), Seq: n}})
		if !recorded || !errors.Is(err, sandbox.ErrJobReplaced) {
			t.Fatalf("the %d'th call of the replaced job = %v, %v; want it listed late, beside "+
				"sandbox.ErrJobReplaced", n, recorded, err)
		}
	}
	logged, err := store.BridgeLog(ctx, next)
	if err != nil {
		t.Fatalf("BridgeLog: %v", err)
	}
	if logged.LateElided != dropped {
		t.Fatalf("the round's log counts %d of the calls it dropped late, want the %d it dropped",
			logged.LateElided, dropped)
	}
	if resumed := runnerResume(resumeInput{Run: next, Bridged: logged}, nil, nil, nil); resumed.LateElided != dropped {
		t.Errorf("the next job's resume hands the runner %d late calls its log dropped, want %d",
			resumed.LateElided, dropped)
	}
}

// A LAUNCHED SESSION'S CALL THAT OUTLIVES ITS JOB IS LISTED IN ITS ROUND. The
// launch binds its session to the job and its round, as the store opened them,
// before the job's box exists, so a call still running when a relaunch carries
// the round on (a delegated worker, here) is listed in the round's log, marked
// late, and the job the row now holds is charged none of it.
//
// Mutation: bind the session to its job without its round, and the call is
// dropped.
func TestALaunchedSessionsCallOutlivingItsJobIsListedInItsRound(t *testing.T) {
	t.Parallel()
	delegate := newHeldTool("delegate")
	run := launchBridged(t, "t-late-round", delegate)
	first := run.row(t, "t-late-round")

	answered := make(chan struct{})
	go func() {
		defer close(answered)
		run.call(t, "delegate")
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case <-delegate.entered:
	case <-ctx.Done():
		t.Fatal("the bridged call never reached its tool")
	}
	next, err := run.pending.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "t-late-round", AgentHandle: "swe", Role: "SWE", Continues: true,
	}, sandbox.Fence{})
	if err != nil || next.Round != first.Round {
		t.Fatalf("the relaunch that carries the round on = round %q, %v; want %q", next.Round, err, first.Round)
	}
	close(delegate.release)
	<-answered

	logged := run.calls(t, "t-late-round")
	if len(logged.Calls) != 1 || logged.Calls[0].Name != "delegate" || !logged.Calls[0].Late {
		t.Fatalf("the round's log lists %+v, want the worker that outlived its job, marked late", logged.Calls)
	}
	if spent := run.row(t, "t-late-round").Launch.Bridged; spent != (sandbox.EngineSpend{}) {
		t.Errorf("the job the row holds is charged %+v of the call that outlived the job before it", spent)
	}
}

// A REOPENED SESSION'S CALL IS LISTED IN ITS ROUND. A node taking a run's seat
// over serves the box again under the job and round off the one row
// ([sandbox.PendingRun.NamedJob]), so a call the box makes after a relaunch
// carried the round on is listed in the round's log, marked late, not dropped.
//
// Mutation: open the reopened session under its job alone, and the call is
// dropped.
func TestAReopenedSessionsLateCallIsListedInItsRound(t *testing.T) {
	t.Parallel()
	e, _ := indicating(t, notify.StatusAlways)
	e.Company().Config.TurnEngine.DelegationDepthLimit = 3
	rig := rigRuns(t, e, nil)
	endpoint := parkOnARun(t, e, rig, "run-reopened")

	dialed := restartBridge(t, e, endpoint)
	row := rig.row(t, "run-reopened")
	e.reopenAgentRun(t.Context(), row)
	next, err := e.sandbox.Load().pending.BeginLaunch(t.Context(), sandbox.PendingRun{TurnID: row.TurnID,
		AgentHandle: row.AgentHandle, Role: row.Role, Continues: true}, sandbox.Fence{})
	if err != nil || next.Round != row.Round {
		t.Fatalf("the relaunch that carries the round on = round %q, %v; want %q", next.Round, err, row.Round)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: dialed, HTTPClient: httpxtest.Pool(t)}, nil)
	if err != nil {
		t.Fatalf("the box could not reach its tools after the recovery: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	called, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: runner.SubmitWorkTool, Arguments: map[string]any{
			"outcome": "blocked", "summary": "nothing to do here",
			"evidence": "this seat holds no write tool",
		},
	})
	if err != nil || called.IsError {
		t.Fatalf("submit_work after the recovery = %+v, %v", called, err)
	}
	logged := rig.calls(t, "run-reopened")
	if len(logged.Calls) != 1 || logged.Calls[0].Name != runner.SubmitWorkTool || !logged.Calls[0].Late {
		t.Fatalf("the round's log lists %+v, want the reopened session's call, marked late", logged.Calls)
	}
}

// replacedStore is a pending store whose every append is of a job a relaunch
// replaced while carrying its round on: listed, and answered so
// ([sandbox.ErrJobReplaced]).
type replacedStore struct {
	sandbox.PendingStore
	mu    sync.Mutex
	asked int
}

func (s *replacedStore) AppendBridgeCall(context.Context, string, sandbox.BridgeAppend) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked++
	return true, sandbox.ErrJobReplaced
}

// A LATE CALL'S UPDATE IS ASKED ONCE AND ANSWERS NO ERROR. The store records
// an update of a job replaced in its round and says the job was replaced: the
// call is in the round's log and its cost reaches no record, by design. Asking
// again writes nothing more, and nothing failed, so the ledger neither asks
// again nor answers an error.
//
// Mutation: read the store's word that the job was replaced as a write that
// failed, and the call is asked again and answers an error.
func TestALateCallsAppendIsAskedOnceAndAnswersNoError(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := &replacedStore{}
		l, _ := readingLedger(store)
		if err := l.Record(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "delegate"}, Seq: 1}); err != nil {
			t.Errorf("a late call's update answered %v, want no error: an error is one its bridge says, and "+
				"an owed one it asks for again", err)
		}
		if store.asked != 1 {
			t.Errorf("a late call's update was asked %d times, want once", store.asked)
		}
	})
}
