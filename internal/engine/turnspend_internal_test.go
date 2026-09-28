package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
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
		Events: []*events.Event{taskWake("task-9", "ENG-9")},
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
	launched := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
	run := sandbox.PendingRun{
		TurnID: "run-1", LaunchID: "launch-1", AgentHandle: "swe",
		WorkItem: "task-9", CreatedAt: launched,
	}
	result := sandbox.Result{Success: true, InputTokens: 900_000, OutputTokens: 40_000}
	spender.RunSpent(t.Context(), run, result)
	spender.RunSpent(t.Context(), run, result)
	run.LaunchID = "launch-2"
	spender.RunSpent(t.Context(), run, result)

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

	// A RUN NO TASK'S TURN LAUNCHED records nothing.
	run.WorkItem, run.LaunchID = "", "launch-3"
	spender.RunSpent(t.Context(), run, result)
	if turns, _ := served.recorded(); len(turns) != 3 {
		t.Fatalf("a run no task woke recorded spend: %+v", turns[3:])
	}
}

// AND THE ENGINE'S OWN COORDINATOR RECORDS IT — driven through
// [Engine.startSandbox] rather than a coordinator this case assembled, because
// a seam declared and never passed is exactly what a test holding its own
// coordinator cannot see. A run that parks on a question is the case: a
// person's answer resumes it and collects nothing, so the collect is the one
// moment its tokens can reach the task.
func TestTheEnginesCoordinatorRecordsWhatARunSpent(t *testing.T) {
	t.Parallel()
	e, served := spendingEngine(t)
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
	completed := types.SandboxRunCompleted{
		AgentHandle: "swe", RoleName: "SWE", TurnID: "run-1",
		LaunchID: run.LaunchID, SandboxID: box.ID(), CodingAgent: "claude-code",
	}
	if err := rt.coordinator.OnCompleted(t.Context(), completed,
		events.New(completed, events.TraceContext{})); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	turns, _ := served.recorded()
	if len(turns) != 1 || turns[0].Task != "task-9" ||
		turns[0].Spend.Input != 500_000 || turns[0].Spend.Output != 20_000 {
		t.Fatalf("the engine's coordinator recorded %+v, want the run's "+
			"500000/20000 tokens on task-9", turns)
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

// servedWriter is a data node's tracker, recording every turn it was handed.
type servedWriter struct {
	estate.TrackerWriter
	mu    sync.Mutex
	turns []tracker.TurnRecord
	ops   []string
}

func (w *servedWriter) RecordTurn(_ context.Context, opID string,
	turn tracker.TurnRecord) (tracker.WriteResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.turns, w.ops = append(w.turns, turn), append(w.ops, opID)
	return tracker.WriteResult{}, nil
}

func (w *servedWriter) recorded() ([]tracker.TurnRecord, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]tracker.TurnRecord(nil), w.turns...), append([]string(nil), w.ops...)
}

// staticRoster names one data node.
type staticRoster struct{ node string }

func (r staticRoster) DataNodes(context.Context) ([]string, error) { return []string{r.node}, nil }
func (staticRoster) Unanswered(string)                             {}

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
	stop, err := estate.Serve(t.Context(), start(), "data-1", func() (estate.Backend, bool) {
		return estate.Backend{
			Writer:      func(estate.Actor) estate.TrackerWriter { return served },
			Established: func(context.Context) bool { return true },
		}, true
	})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	q := start()
	client, err := estate.NewClient(estate.ClientOptions{
		Queue: q, Roster: staticRoster{node: "data-1"}, Self: "agent-1",
	})
	if err != nil {
		t.Fatalf("estate client: %v", err)
	}

	seat := &org.Role{Name: "SWE", DeclaredHandle: "swe", LLM: org.ProviderKeys{"only"}}
	organization := &org.Organization{Name: "Acme", Roles: []*org.Role{seat}}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "only", Provider: billingModel{}}})
	if err != nil {
		t.Fatalf("phase.NewRegistry: %v", err)
	}
	e := &Engine{backends: &Backends{Queue: q}, estate: client}
	e.remote.Store(&remoteNative{client: client, tracker: true})
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
