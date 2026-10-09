package engine_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// AN ANSWER BY TURN HELD BEHIND ITS RUN'S OWN NEW JOB NEVER ANSWERS THAT JOB'S
// QUESTION, end to end through the seat's inbox.
//
//  1. A run parks on question Q1, and two people answer Q1 by turn.
//  2. The first answer resumes the run, and the resumed turn calls
//     run_sandbox again: the seat is busy, so its inbox is held, and the
//     second answer waits on it.
//  3. The new job parks on question Q2, which lifts the hold — and the second
//     answer is the first thing the seat is offered.
//
// The second answer was given against Q1, which nobody is waiting on any more.
// Named only by its run, it claimed whatever the run waited on by then and
// resumed it a second time, with Q1's answer presented as Q2's. It names its
// question now, and is spent as not_awaiting.
func TestAnAnswerByTurnHeldBehindItsRunsNextJobDoesNotAnswerItsNextQuestion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })

	store := sandbox.NewCoordStore(coordmemory.NewFleet())
	provider, runner := sandbox.NewFakeProvider(), sandbox.NewFakeRunner("claude-code")
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: provider},
		Runners:   map[string]sandbox.Runner{"claude-code": runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	resumer := &relaunchingResumer{manager: manager, store: store}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Audience: noAudience{}, Queue: q, Pending: store, Manager: manager,
		Resume: resumer, Hold: inboxHold{q: q}, After: (&handCranked{}).after,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	resumer.coordinator = coordinator

	// ROUTED, not merely dispatched: an answer that raced the seat's hold is
	// deferred by the screening before it reaches the run, and comes round
	// again once the hold lifts.
	var routed atomic.Int32
	ordinary := &turns{}
	d := &engine.Dispatcher{
		Ledgered:    func(kind string) bool { return kind == notificationType },
		Turn:        ordinary.run,
		Completions: ledgerstore.NewMemoryCompletions(),
		Answer:      coordinator.TryResumeFromAnswer,
		AnswerByTurn: func(ctx context.Context, given types.SandboxAnswerGiven,
			trigger *events.Event) (sandbox.AnswerDisposition, error) {
			routed.Add(1)
			return coordinator.AnswerByTurn(ctx, given, trigger)
		},
		HoldSandbox: coordinator.HoldSeat,
		Conditions: func(handle string) inbox.Conditions {
			held, awaits := coordinator.SeatRuns(handle)
			return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true,
				SeatHeldBySandbox: held, SandboxAwaitsAnswer: awaits}
		},
		NoteDeferred: func(string) {},
	}
	if err := q.Subscribe(ctx, topics.AgentInbox("swe"), topics.AgentInboxGroup("swe"),
		func(ctx context.Context, ev *events.Event) queue.Result {
			return d.Dispatch(ctx, "swe", []*events.Event{ev})
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// THE RUN PARKS ON Q1.
	if _, err := coordinator.Launch(ctx, manager, resumer.launch("t1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, err := store.MarkSuspended(ctx, "t1", sandbox.Suspension{State: []byte(`{"messages":[]}`)}); err != nil {
		t.Fatalf("MarkSuspended: %v", err)
	}
	runner.Finish(sandbox.Result{NeedsInput: true, Question: "which branch?", AskTo: "requester"})
	complete(t, coordinator, store, "t1")
	q1 := mustRun(t, store, "t1")
	if q1.Status != sandbox.StatusAwaiting {
		t.Fatalf("the run is %q, want it parked on its first question", q1.Status)
	}

	// THE FIRST ANSWER RESUMES IT, and the resumed turn starts another job.
	first, late := answerAgainst(q1, "use the release branch"), answerAgainst(q1, "use main")
	if err := q.Publish(ctx, topics.AgentInbox("swe"), first); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	eventually(t, "the first answer to resume the run", func() bool { return resumer.count() == 1 })
	if !coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the premise: the resumed turn's new job holds the seat")
	}

	// THE SECOND ANSWER TO Q1 WAITS BEHIND THE JOB.
	if err := q.Publish(ctx, topics.AgentInbox("swe"), late); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	settle()
	if n := resumer.count(); n != 1 || routed.Load() != 1 {
		t.Fatalf("the run was resumed %d times, and %d answers routed to it, while its new job "+
			"held the seat", n, routed.Load())
	}

	// THE NEW JOB PARKS ON Q2, the hold lifts, and the late answer is offered.
	runner.Finish(sandbox.Result{NeedsInput: true, Question: "which test suite?", AskTo: "requester"})
	complete(t, coordinator, store, "t1")
	eventually(t, "the late answer to be routed once the seat is free", func() bool {
		return routed.Load() == 2
	})
	settle()
	if n := resumer.count(); n != 1 {
		t.Fatalf("the run was resumed %d times: the late answer to Q1 resumed it as the answer to Q2", n)
	}
	q2 := mustRun(t, store, "t1")
	if q2.Status != sandbox.StatusAwaiting || q2.Question != "which test suite?" || q2.LaunchID == q1.LaunchID {
		t.Fatalf("run = %q on %q, want it still waiting on its second question", q2.Status, q2.Question)
	}
	if ran := ordinary.ran(); len(ran) != 0 {
		t.Fatalf("an answer by turn was run as a turn: %q", ran)
	}
}

// relaunchingResumer re-enters a parked run as a coding agent whose turn calls
// run_sandbox again would: it takes the answer, and launches the next job.
type relaunchingResumer struct {
	coordinator *sandbox.Coordinator
	manager     *sandbox.Manager
	store       *sandbox.CoordStore

	mu    sync.Mutex
	calls int
}

func (r *relaunchingResumer) launch(turnID string) sandbox.LaunchRequest {
	return sandbox.LaunchRequest{
		Turn:  sandbox.TurnRef{TurnID: turnID, AgentID: "a-1", AgentHandle: "swe", Role: "SWE"},
		Brief: "fix the flaky test", Task: "get CI green",
		Spec: sandbox.Spec{CodingAgent: "claude-code", PauseTTLSec: 1800},
	}
}

func (r *relaunchingResumer) Resume(ctx context.Context, req sandbox.ResumeRequest) error {
	if req.Begin != nil {
		if err := req.Begin(ctx); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	if _, err := r.coordinator.Launch(ctx, r.manager, r.launch(req.Run.TurnID)); err != nil {
		return err
	}
	_, err := r.store.MarkSuspended(ctx, req.Run.TurnID, sandbox.Suspension{State: []byte(`{"messages":[]}`)})
	return err
}

func (r *relaunchingResumer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// answerAgainst is an operator's answer to the question run is waiting on, as
// answer_run puts it on the seat's inbox.
func answerAgainst(run sandbox.PendingRun, answer string) *events.Event {
	ev := events.New(types.SandboxAnswerGiven{
		TurnID: run.TurnID, AgentHandle: run.AgentHandle, Answer: answer,
		AnsweredBy: "founder-token", AnsweredBySeat: "founder",
		LaunchID: run.LaunchID,
	}, events.TraceContext{})
	ev.Source = types.OperatorSource
	return ev
}

// complete hands the coordinator the completion of the job the run holds now.
func complete(t *testing.T, c *sandbox.Coordinator, store *sandbox.CoordStore, turnID string) {
	t.Helper()
	run := mustRun(t, store, turnID)
	done := types.SandboxRunCompleted{Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		TurnID: turnID, LaunchID: run.LaunchID, SandboxID: run.SandboxID, CodingAgent: run.CodingAgent}
	if err := c.OnCompleted(t.Context(), done, events.New(done, events.TraceContext{})); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
}

// mustRun is one run's row.
func mustRun(t *testing.T, store *sandbox.CoordStore, turnID string) sandbox.PendingRun {
	t.Helper()
	run, found, err := store.Get(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("Get %s = %v, %v", turnID, found, err)
	}
	return run
}
