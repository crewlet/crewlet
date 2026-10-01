package engine

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A TURN THE TRACKER WOKE ABOUT A TASK ADDS WHAT IT COST TO THAT TASK.
//
// The whole defect: the tracker's per-task spend had a writer, an applier, a
// schema and readers, and no turn ever recorded anything — so every task in
// every company reported it had cost nothing. This drives the real runTurn on a
// node WITHOUT the data role, so the spend crosses the estate wire to a data
// node exactly as a stateless node's does, and asserts what arrived: the task
// the wake named, one turn, the rounds it ran, and the tokens its model billed
// — the cached share among them.
func TestATurnWokenAboutATaskRecordsItsSpendOnIt(t *testing.T) {
	t.Parallel()
	e, served := spendingEngine(t)
	if _, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1",
		WorkSince: time.Now().UTC(),
		Events:    []*events.Event{taskWake("task-9", "ENG-9")},
	}); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	turns, ops := served.recorded()
	if len(turns) != 1 {
		t.Fatalf("the turn recorded %d spend(s), want one", len(turns))
	}
	got := turns[0]
	if got.Task != "task-9" || got.Seat != "swe" || got.TurnID != "run-1" {
		t.Fatalf("the spend was recorded as %+v, want task-9 by swe in run-1", got)
	}
	if got.Spend.Turns != 1 || got.Spend.Rounds != 1 {
		t.Fatalf("the spend counts %d turn(s) and %d round(s), want 1 and 1",
			got.Spend.Turns, got.Spend.Rounds)
	}
	// TWO PHASES, each billed by the model below.
	if got.Spend.Input != 2*billedInput || got.Spend.Output != 2*billedOutput ||
		got.Spend.CacheRead != 2*billedCached {
		t.Fatalf("the spend is %+v, want the two phases' tokens (%d in, %d out, "+
			"%d of them cached)", got.Spend, 2*billedInput, 2*billedOutput, 2*billedCached)
	}
	if ops[0] == "" {
		t.Fatal("the spend crossed with no operation id, so a repeat would count it twice")
	}

	// THE SAME TURN'S RESUMED HALF records again under its own operation,
	// counting its rounds past the one it re-entered and not the turn.
	started := time.Now()
	resumed := turnRecordOf(turnTelemetry{
		handle: "swe", runID: "run-1", workItem: "task-9", resumedRound: 1,
		startedAt: started,
	}, runner.Spend{InputTokens: 5}, turn.Result{Rounds: 3}, started)
	if resumed.Spend.Turns != 0 || resumed.Spend.Rounds != 2 {
		t.Fatalf("a resumed half re-entering round 1 and ending at 3 records "+
			"%d turn(s) and %d round(s), want 0 and 2", resumed.Spend.Turns,
			resumed.Spend.Rounds)
	}
}

// A TURN NO TASK WOKE RECORDS NOTHING, however many tasks its tools touch: a
// chat message's turn was not spent on the work items it happened to file, and
// charging them by the calls a model chose to make would make the counter a
// record of the model's choices.
func TestATurnNoTaskWokeRecordsNoSpend(t *testing.T) {
	t.Parallel()
	e, served := spendingEngine(t)
	chat := events.New(types.ExternalNotification{
		NotificationSource: "slack", SourceEventType: "message",
		Sender: "ana", Subject: "a message", Body: "can you look at this",
	}, events.TraceContext{})
	if _, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-2", Events: []*events.Event{chat},
		WorkSince: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if turns, _ := served.recorded(); len(turns) != 0 {
		t.Fatalf("a chat turn recorded spend on %+v", turns)
	}
	// NOR DOES A WAKE ABOUT A PERSON'S PRIORITY LIST, whose item names the
	// task that reached its top rather than the work the turn is about.
	if item := workItemOf([]*events.Event{priorityWake("task-3")}); item != "" {
		t.Fatalf("a priorities wake was read as a turn spent on %q", item)
	}
}

// WHAT A CODING RUN SPENT INSIDE ITS BOX LANDS ON THE TASK TOO — once per
// launch, however many times its completion is collected.
//
// No phase carries it: the turn was suspended while the box worked, so without
// this the task a coding agent spent a million tokens on reported the few
// thousand its executor spent writing the brief. Each collect of one launch
// derives one operation, so a completion retried after a failed resume counts
// nothing more; a second launch in the same turn is its own.
func TestACollectedRunsTokensLandOnItsTaskOncePerLaunch(t *testing.T) {
	t.Parallel()
	e, served := spendingEngine(t)
	spender := runSpender{engine: e}
	// THE ROW IS FORTY DAYS OLDER THAN THE COLLECT — a turn parked on a
	// question past the operation ledger's retention and launched again —
	// and the operation is still minted at the collect.
	collected := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
	run := sandbox.PendingRun{
		TurnID: "run-1", LaunchID: "launch-1", AgentHandle: "swe",
		WorkItem: "task-9", CreatedAt: collected.Add(-40 * 24 * time.Hour),
		CollectedAt: collected,
	}
	result := sandbox.Result{Success: true, InputTokens: 900_000, OutputTokens: 40_000}
	spent := func() {
		t.Helper()
		if err := spender.RunSpent(t.Context(), run, result); err != nil {
			t.Fatalf("RunSpent: %v", err)
		}
	}
	spent()
	spent()
	run.LaunchID = "launch-2"
	spent()

	turns, ops := served.recorded()
	if len(turns) != 3 {
		t.Fatalf("three collects recorded %d spend(s), want three", len(turns))
	}
	got := turns[0]
	if got.Task != "task-9" || got.Seat != "swe" || got.TurnID != "run-1" {
		t.Fatalf("the run's spend was recorded as %+v, want task-9 by swe in run-1", got)
	}
	if want := (tracker.TurnSpend{Input: 900_000, Output: 40_000}); got.Spend != want {
		t.Fatalf("the run's spend is %+v, want its tokens and no turn, round or "+
			"wall-clock of its own (%+v)", got.Spend, want)
	}
	if ops[0] != ops[1] {
		t.Fatalf("two collects of one launch wrote operations %q and %q, so the "+
			"retry counts the run twice", ops[0], ops[1])
	}
	if ops[2] == ops[0] {
		t.Fatal("a second launch in the same turn reused the first's operation, " +
			"so its spend is never counted")
	}
	// MINTED AT THE FIRST COLLECT, the first moment it is attempted: minted at
	// the row's creation, a ledger swept past that instant answered it
	// `unknown` on every node and never published it.
	if minted, ok := statelog.OpMintedAt(ops[0]); !ok || !minted.Equal(collected) {
		t.Fatalf("the run's spend is minted at %v (%v), want its first collect %v",
			minted, ok, collected)
	}

	// AND A RUN WITH NO COLLECT INSTANT is refused rather than minted at the
	// zero instant, which every ledger that has lost anything cannot vouch for.
	stray := run
	stray.CollectedAt, stray.LaunchID = time.Time{}, "launch-stray"
	if err := spender.RunSpent(t.Context(), stray, result); err == nil {
		t.Fatal("a run's spend with no collect instant was written")
	}
	if turns, _ := served.recorded(); len(turns) != 3 {
		t.Fatalf("a run's spend with no collect instant reached the task: %+v", turns[3:])
	}

	// A RUN NO TASK'S TURN LAUNCHED records nothing.
	run.WorkItem, run.LaunchID = "", "launch-3"
	spent()
	if turns, _ := served.recorded(); len(turns) != 3 {
		t.Fatalf("a run no task woke recorded spend: %+v", turns[3:])
	}
}

// A COLLECTED RUN'S SPEND SAYS WHETHER ITS FATE IS SETTLED, because the
// collect holds the run until it is: a write that landed, and one refused
// because the task was purged — which no retry changes — are settled, while one
// refused for now or answered with an outcome nobody knows is not, and the
// collect's retry repeats it under the same operation. Read the other way, a
// purged task would hold its run for ever, and a write that never answered
// would leave the run's tokens off its task for good.
func TestACollectedRunsSpendSaysWhetherItsFateIsSettled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		answer  func() (tracker.WriteResult, error)
		settled bool
	}{
		{"applied", func() (tracker.WriteResult, error) {
			return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
		}, true},
		{"pending on the log", func() (tracker.WriteResult, error) {
			return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomePending}}, nil
		}, true},
		{"the task was purged", func() (tracker.WriteResult, error) {
			return tracker.WriteResult{}, fmt.Errorf("%w: task task-9 was purged, and "+
				"nothing it cost can be recorded against it", tracker.ErrNoTask)
		}, true},
		{"a task no record on its log creates", func() (tracker.WriteResult, error) {
			return tracker.WriteResult{}, fmt.Errorf("%w: no record on the log creates "+
				"task task-9, so nothing it cost can be recorded against it", tracker.ErrNoTask)
		}, true},
		{"an outcome nobody knows", func() (tracker.WriteResult, error) {
			return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeUnknown}}, nil
		}, false},
		// AN UNKNOWN THE ANSWERING NODE'S LEDGER CANNOT VOUCH FOR is answered
		// the same way on every repeat there, so it is settled — unknown, and
		// said so — rather than holding the run for ever.
		{"an unknown no ledger here can vouch for", func() (tracker.WriteResult, error) {
			return tracker.WriteResult{Result: statelog.Result{
				Outcome: statelog.OutcomeUnknown, Unvouched: true}}, nil
		}, true},
		{"refused for now", func() (tracker.WriteResult, error) {
			return tracker.WriteResult{}, fmt.Errorf("tracker: task task-9 is not on "+
				"this node: %w", statelog.ErrUnavailable)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, served := spendingEngine(t)
			served.answering(tc.answer)
			at := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
			err := runSpender{engine: e}.RunSpent(t.Context(), sandbox.PendingRun{
				TurnID: "run-1", LaunchID: "launch-1", AgentHandle: "swe",
				WorkItem: "task-9", CreatedAt: at, CollectedAt: at,
			}, sandbox.Result{Success: true, InputTokens: 900})
			if turns, _ := served.recorded(); len(turns) != 1 {
				t.Fatalf("the run's spend reached the task %d time(s), want once", len(turns))
			}
			if settled := err == nil; settled != tc.settled {
				t.Fatalf("RunSpent = %v, want settled=%v", err, tc.settled)
			}
		})
	}
}

// AND THE ENGINE'S OWN COORDINATOR RECORDS IT — driven through
// [Engine.startSandbox] rather than a coordinator this case assembled, because
// a seam declared and never passed is exactly what a test holding its own
// coordinator cannot see. A run that parks on a question is the case: a
// person's answer resumes it and collects nothing, so the collect is the one
// moment its tokens can reach the task.
//
// AND THE COLLECT HOLDS ONLY ON AN ANSWER A RETRY CAN CHANGE. A lost
// acknowledgement holds it, since the retry settles it under the same
// operation; a task no record on its log creates, and an unknown the answering
// node's ledger cannot vouch for, are answered the same way on every retry,
// and holding on them held the seat for ever.
func TestTheEnginesCoordinatorRecordsWhatARunSpent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer func() (tracker.WriteResult, error)
		held   bool
	}{
		{"recorded", nil, false},
		{"an acknowledgement lost", func() (tracker.WriteResult, error) {
			return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeUnknown}}, nil
		}, true},
		{"a task no record on its log creates", func() (tracker.WriteResult, error) {
			return tracker.WriteResult{}, fmt.Errorf("%w: no record on the log creates "+
				"task task-9, so nothing it cost can be recorded against it", tracker.ErrNoTask)
		}, false},
		{"an unknown no ledger here can vouch for", func() (tracker.WriteResult, error) {
			return tracker.WriteResult{Result: statelog.Result{
				Outcome: statelog.OutcomeUnknown, Unvouched: true}}, nil
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, served := spendingEngine(t)
			if tc.answer != nil {
				served.answering(tc.answer)
			}
			rt, run, completed := aParkingRun(t, e)
			err := rt.coordinator.OnCompleted(t.Context(), completed,
				events.New(completed, events.TraceContext{}))
			turns, _ := served.recorded()
			if len(turns) != 1 || turns[0].Task != "task-9" ||
				turns[0].Spend.Input != 500_000 || turns[0].Spend.Output != 20_000 {
				t.Fatalf("the engine's coordinator recorded %+v, want the run's "+
					"500000/20000 tokens on task-9", turns)
			}
			after, _, getErr := rt.pending.Get(t.Context(), run.TurnID)
			if getErr != nil {
				t.Fatalf("Get: %v", getErr)
			}
			switch {
			case tc.held && (err == nil || after.Status != sandbox.StatusRunning):
				t.Fatalf("OnCompleted = %v leaving %q, want the collect held for "+
					"its retry, back in %q", err, after.Status, sandbox.StatusRunning)
			case !tc.held && (err != nil || !slices.Contains(sandbox.Awaiting, after.Status)):
				t.Fatalf("OnCompleted = %v leaving %q, want the run parked on its "+
					"question (%v) — no retry changes this answer", err, after.Status,
					sandbox.Awaiting)
			}
		})
	}
}

// aParkingRun brings the engine's sandbox runtime up over a fake provider and
// leaves one suspended run of task-9's turn whose job has finished asking a
// question, with the completion that collects it.
func aParkingRun(t *testing.T, e *Engine) (*sandboxRuntime, sandbox.PendingRun,
	types.SandboxRunCompleted) {

	t.Helper()
	e.backends.Fleet = coordmem.NewFleet()
	provider, runner := sandbox.NewFakeProvider(), sandbox.NewFakeRunner("claude-code")
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: provider},
		Runners:   map[string]sandbox.Runner{"claude-code": runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if started, err := e.startSandbox(t.Context(), manager); err != nil || !started {
		t.Fatalf("startSandbox = (%v, %v), want a runtime brought up", started, err)
	}
	rt := e.sandbox.Load()
	box, err := provider.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := rt.pending.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "run-1", AgentHandle: "swe", Role: "SWE", WorkItem: "task-9",
		Placement: string(sandbox.Direct),
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := rt.pending.AttachSandbox(t.Context(), "run-1", sandbox.BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", CodingAgent: "claude-code",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	if ok, err := rt.pending.MarkSuspended(t.Context(), "run-1", map[string]any{
		"version": float64(1), "pending_tool_call_id": "call-1",
		"pending_tool_name": "run_sandbox",
	}); err != nil || !ok {
		t.Fatalf("MarkSuspended = %v, %v", ok, err)
	}
	run, _, err := rt.pending.Get(t.Context(), "run-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	runner.Finish(sandbox.Result{NeedsInput: true, Question: "which branch?",
		AskTo: "requester", InputTokens: 500_000, OutputTokens: 20_000})
	return rt, run, types.SandboxRunCompleted{
		AgentHandle: "swe", RoleName: "SWE", TurnID: "run-1",
		LaunchID: run.LaunchID, SandboxID: box.ID(), CodingAgent: "claude-code",
	}
}

const (
	billedInput  = 1200
	billedOutput = 90
	billedCached = 800
)

// billingModel answers each phase with its own submission and bills it.
type billingModel struct{}

func (billingModel) Model() string { return "billing" }

func (billingModel) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	name, args := runner.SubmitWorkTool, map[string]any{
		"outcome": "blocked", "summary": "nothing to do here",
		"evidence": "this seat holds no write tool",
	}
	for _, tool := range req.Tools {
		if tool.Name == runner.SubmitReviewTool {
			name, args = runner.SubmitReviewTool, map[string]any{"decision": "done"}
		}
	}
	return &llm.Completion{
		Model:       "billing",
		ToolCalls:   []llm.ToolCall{{ID: "c1", Name: name, Arguments: args}},
		InputTokens: billedInput, OutputTokens: billedOutput, CacheRead: billedCached,
	}, nil
}

// servedWriter is a data node's tracker, recording every turn it was handed
// and answering each with answer — applied when it is unset.
type servedWriter struct {
	estate.TrackerWriter
	mu     sync.Mutex
	turns  []tracker.TurnRecord
	ops    []string
	answer func() (tracker.WriteResult, error)
}

func (w *servedWriter) RecordTurn(_ context.Context, opID string,
	turn tracker.TurnRecord) (tracker.WriteResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.turns, w.ops = append(w.turns, turn), append(w.ops, opID)
	if w.answer != nil {
		return w.answer()
	}
	return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

// answering makes every later write answer with answer.
func (w *servedWriter) answering(answer func() (tracker.WriteResult, error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.answer = answer
}

func (w *servedWriter) recorded() ([]tracker.TurnRecord, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]tracker.TurnRecord(nil), w.turns...), append([]string(nil), w.ops...)
}

// staticRoster names one data node.
type staticRoster struct{ node string }

func (r staticRoster) LiveDataNodes() ([]string, error) { return []string{r.node}, nil }
func (staticRoster) Invalidate()                        {}

// servedEstate is a data node serving the whole estate from one backend.
type servedEstate struct {
	backend estate.Backend
	cpus    estate.CPUs
}

func (s *servedEstate) For(context.Context, statelog.PartitionID) (estate.Backend, bool, error) {
	return s.backend, true, nil
}

func (s *servedEstate) CPUs() *estate.CPUs { return &s.cpus }

// spendingEngine is an engine WITHOUT the data role running one seat on the
// native tracker, whose writes are served by one data node over the estate.
func spendingEngine(t *testing.T) (*Engine, *servedWriter) {
	t.Helper()
	broker := memory.NewBroker()
	start := func() *memory.Queue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
		return q
	}
	served := &servedWriter{}
	placement := partmap.Whole{Running: LayoutZero(), Roster: staticRoster{node: "data-1"}}
	stop, err := estate.Serve(t.Context(), start(), "data-1", &servedEstate{backend: estate.Backend{
		Writer: func(estate.Actor) estate.TrackerWriter { return served },
	}}, placement, estate.ServerSeams{})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	q := start()
	router, err := estate.NewRouter(estate.RouterOptions{
		Queue: q, Placement: placement, Self: "agent-1", Session: estate.NewSession(),
	})
	if err != nil {
		t.Fatalf("estate router: %v", err)
	}

	seat := &org.Role{Name: "SWE", DeclaredHandle: "swe", LLM: org.ProviderKeys{"only"}}
	organization := &org.Organization{Name: "Acme", Roles: []*org.Role{seat}}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "only", Provider: billingModel{}}})
	if err != nil {
		t.Fatalf("phase.NewRegistry: %v", err)
	}
	e := &Engine{backends: &Backends{Queue: q}, router: router}
	e.remote.Store(&remoteNative{tracker: true})
	e.epoch.current.Store(&Company{
		Org: organization, Models: models, Tools: tools.NewRegistry(),
		Config: &config.Company{Name: "Acme", TurnEngine: config.TurnEngine{
			MaxIterations: 1, DelegationDepthLimit: 1, MaxToolRounds: 3,
		}},
	})
	e.notify.registry = notify.NewRegistry(organization)
	return e, served
}

// taskWake is the change feed's wake about one task, as the tracker's parser
// stamps it.
func taskWake(id, key string) *events.Event {
	ev := events.New(types.ExternalNotification{
		NotificationSource: tracker.Source, SourceEventType: "assigned",
		Sender: "lead", Subject: key + " assigned: a task",
		Metadata: map[string]string{
			tracker.MetaTaskID: id, tracker.MetaTaskKey: key,
			tracker.MetaObject: string(tracker.KindTask), tracker.MetaObjectID: id,
			tracker.MetaVia: "assignee",
		},
	}, events.TraceContext{})
	ev.Source = "notify." + tracker.Source
	return ev
}

// priorityWake is the wake about somebody's priority list, naming the task
// that reached its top.
func priorityWake(taskID string) *events.Event {
	return events.New(types.ExternalNotification{
		NotificationSource: tracker.Source, SourceEventType: "prioritised",
		Metadata: map[string]string{
			tracker.MetaTaskID: taskID,
			tracker.MetaObject: string(tracker.KindPerson), tracker.MetaObjectID: "swe",
		},
	}, events.TraceContext{})
}
