package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A TURN ON A NODE WITHOUT `data` CHARGES THE TASK THAT WOKE IT, ON A DATA
// NODE: the spend is a tracker write like any other, so it crosses the estate
// wire, and what arrives is the task the wake named, one turn, and the tokens
// its model billed — the cached share among them — under an operation id a
// repeat collapses on.
//
// The per-segment arithmetic of a charge is turnspend_internal_test.go's; this
// is the one thing only a stateless node can show: that the charge is not left
// behind on a node that keeps no tracker. And the executor's share of it is its
// run's, collected from the box — the cached share included, which a CLI
// reports beside its tokens and the charge carries like every phase's.
//
// Mutation: leave the job's cached share out of the resumed segment's charge,
// and CacheRead is the reviewer's alone.
func TestAStatelessNodesTurnChargesItsTaskOnADataNode(t *testing.T) {
	t.Parallel()
	e, served := spendingEngine(t)
	rig := rigRuns(t, e, modelPlayer(t, billingRun))
	runToItsEnd(t, e, rig, Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1",
		WorkSince: time.Now().UTC(),
		Events:    []*events.Event{taskWake("task-9", "ENG-9")},
	})
	// ONE CHARGE PER SEGMENT: the dispatch that parked on the launch, and
	// the resume that collected the run and reviewed it (ADR-0022).
	turns, ops := served.recorded()
	if len(turns) != 2 {
		t.Fatalf("the turn recorded %d charge(s) on the data node, want its two segments': %+v",
			len(turns), turns)
	}
	var whole worklet.TurnSpend
	for _, got := range turns {
		if got.Task != "task-9" || got.Seat != "swe" || got.TurnID != "run-1" {
			t.Fatalf("a charge arrived as %+v, want task-9 by swe in run-1", got)
		}
		whole.Turns += got.Spend.Turns
		whole.Input += got.Spend.Input
		whole.Output += got.Spend.Output
		whole.CacheRead += got.Spend.CacheRead
	}
	if whole.Turns != 1 || whole.Input != 2*billedInput ||
		whole.Output != 2*billedOutput || whole.CacheRead != 2*billedCached {
		t.Fatalf("the segments charged %+v between them, want one turn and the run's and the "+
			"review's tokens (%d in, %d out, %d of them cached)", whole,
			2*billedInput, 2*billedOutput, 2*billedCached)
	}
	if ops[0] == "" || ops[1] == "" || ops[0] == ops[1] {
		t.Fatalf("the charges crossed under operation ids %q: each segment needs its own, or a "+
			"repeat would count it twice and the second segment would collapse into the first", ops)
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
	rig := rigRuns(t, e, modelPlayer(t, billingRun))
	runToItsEnd(t, e, rig, Request{
		Handle: "swe", WorkKey: "wk-2", RunID: "run-2", Events: []*events.Event{chat},
		WorkSince: time.Now().UTC(),
	})
	if turns, _ := served.recorded(); len(turns) != 0 {
		t.Fatalf("a chat turn charged %+v", turns)
	}
}

const (
	billedInput  = 1200
	billedOutput = 90
	billedCached = 800
)

// billingModel answers each phase with its own submission and bills it: a
// coding CLI on the real session whose one request calls the phase's
// terminator — submit_review where the phase offers it, submit_work otherwise.
func billingModel() phasedCLI {
	return phasedCLI{model: "billing", pick: billingRun}
}

// billingRun is billingModel's run for a phase offering offered.
func billingRun(offered []string) llmtest.Run {
	name, args := runner.SubmitWorkTool, map[string]any{
		"outcome": "blocked", "summary": "nothing to do here",
		"evidence": "this seat holds no write tool",
	}
	if offers(offered, runner.SubmitReviewTool) {
		name, args = runner.SubmitReviewTool, map[string]any{"decision": "done"}
	}
	return calling(name, args, llm.Usage{
		Input: billedInput, Output: billedOutput, CacheRead: billedCached,
	})
}

// servedWriter is a data node's tracker, recording every turn it was handed.
type servedWriter struct {
	estate.TrackerWriter
	mu    sync.Mutex
	turns []worklet.TurnRecord
	ops   []string
}

func (w *servedWriter) RecordTurn(_ context.Context, opID string,
	turn worklet.TurnRecord) (worklet.WriteResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.turns, w.ops = append(w.turns, turn), append(w.ops, opID)
	return worklet.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (w *servedWriter) recorded() ([]worklet.TurnRecord, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]worklet.TurnRecord(nil), w.turns...), append([]string(nil), w.ops...)
}

// staticRoster names one data node.
type staticRoster struct{ node string }

func (r staticRoster) LiveDataNodes() ([]string, error) { return []string{r.node}, nil }
func (staticRoster) Invalidate()                        {}

// servedEstate is a data node serving the estate from one backend.
type servedEstate struct {
	backend estate.Backend
}

func (s *servedEstate) For(context.Context, string) (estate.Backend, bool) {
	return s.backend, true
}

// spendingEngine is an engine WITHOUT the data role running one seat on the
// Worklet, whose writes are served by one data node over the estate. Its seat's
// executor launches on [runnerKey]; the case rigs what plays the run.
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
	envelopes := routerEnvelopes(t)
	stop, err := estate.Serve(t.Context(), start(), estate.ServerOptions{
		Self: "data-1", Envelopes: envelopes,
		Local: &servedEstate{backend: estate.Backend{
			Writer: func(estate.Actor) estate.TrackerWriter { return served },
		}},
	})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	q := start()
	router, err := estate.NewRouter(estate.RouterOptions{
		Queue: q, Placement: placement, Git: noGitNodes{}, Self: "agent-1",
		Session:   estate.NewSession(),
		Timing:    estate.Production(),
		Envelopes: envelopes,
	})
	if err != nil {
		t.Fatalf("estate router: %v", err)
	}

	seat := &org.Role{Name: "SWE", DeclaredHandle: "swe", LLM: org.ProviderKeys{"only"}}
	organization := &org.Organization{Name: "Acme", Roles: []*org.Role{seat}}
	models := runnable(t, []phase.Entry{{Key: "only", Provider: billingModel()}}, seat)
	// IN NORMAL MODE, as [New] starts a node unless told otherwise: the mode
	// is what decides whether this node hands out a writer at all, a routed
	// one included ([Engine.writers]), and the charge is a write.
	e := withSessions(t, &Engine{backends: &Backends{Queue: q, Fleet: coordmem.NewFleet()},
		router: router, mode: statelog.ModeNormal})
	e.remote.Store(&remoteNative{tracker: true})
	e.epoch.current.Store(&Company{
		Org: organization, Models: models, Tools: tools.NewRegistry(),
		Config: &config.Company{Name: "Acme", TurnEngine: config.TurnEngine{
			MaxIterations: 1, DelegationDepthLimit: 1,
		}},
	})
	e.notify.registry = notify.NewRegistry(organization, nil)
	return e, served
}

// taskWake is the change feed's wake about one task, as the tracker's parser
// stamps it.
func taskWake(id, key string) *events.Event {
	ev := events.New(types.ExternalNotification{
		NotificationSource: worklet.Source, SourceEventType: "assigned",
		Sender: "lead", Subject: key + " assigned: a task",
		Metadata: map[string]string{
			worklet.MetaTaskID: id, worklet.MetaTaskKey: key,
			worklet.MetaObject: string(worklet.KindTask), worklet.MetaObjectID: id,
			worklet.MetaVia: "assignee",
		},
	}, events.TraceContext{})
	ev.Source = "notify." + worklet.Source
	return ev
}
