package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A TURN ON A NODE WITHOUT `data` CHARGES THE TASK THAT WOKE IT, ON A DATA
// NODE: the spend is a tracker write like any other, so it crosses the estate
// wire, and what arrives is the task the wake named, one turn, and the tokens
// its model billed — the cached share among them — under an operation id a
// repeat collapses on.
//
// The per-segment arithmetic of a charge is turnspend_internal_test.go's; this
// is the one thing only a stateless node can show: that the charge is not left
// behind on a node that keeps no tracker.
func TestAStatelessNodesTurnChargesItsTaskOnADataNode(t *testing.T) {
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
		t.Fatalf("the turn recorded %d charge(s) on the data node, want one", len(turns))
	}
	got := turns[0]
	if got.Task != "task-9" || got.Seat != "swe" || got.TurnID != "run-1" {
		t.Fatalf("the charge arrived as %+v, want task-9 by swe in run-1", got)
	}
	if got.Spend.Turns != 1 || got.Spend.Input != 2*billedInput ||
		got.Spend.Output != 2*billedOutput || got.Spend.CacheRead != 2*billedCached {
		t.Fatalf("the charge is %+v, want one turn and the two phases' tokens (%d in, "+
			"%d out, %d of them cached)", got.Spend, 2*billedInput, 2*billedOutput, 2*billedCached)
	}
	if ops[0] == "" {
		t.Fatal("the charge crossed with no operation id, so a repeat would count it twice")
	}
}

// A TURN NOTHING NAMED AN ITEM FOR, AND THAT WROTE NONE, CHARGES NOTHING on any
// node — a chat message's turn on a stateless node included.
func TestAStatelessNodesUnattributedTurnChargesNothing(t *testing.T) {
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
		t.Fatalf("a chat turn charged %+v", turns)
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
	return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
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

// servedEstate is a data node serving the estate from one backend.
type servedEstate struct {
	backend estate.Backend
}

func (s *servedEstate) For(context.Context) (estate.Backend, bool) {
	return s.backend, true
}

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
	placement := wholeEstate{roster: staticRoster{node: "data-1"}}
	stop, err := estate.Serve(t.Context(), start(), "data-1", &servedEstate{backend: estate.Backend{
		Writer: func(estate.Actor) estate.TrackerWriter { return served },
	}}, estate.ServerSeams{})
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
