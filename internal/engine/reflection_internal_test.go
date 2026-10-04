package engine

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// reflectProbe is a reflection worker that records the turns it ran on.
type reflectProbe struct {
	mu    sync.Mutex
	turns []string
}

func (p *reflectProbe) Name() string              { return "probe" }
func (p *reflectProbe) Skip(learning.Turn) string { return "" }
func (p *reflectProbe) Reflect(_ context.Context, t learning.Turn) ([]events.Payload, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.turns = append(p.turns, t.Event.TurnID)
	return nil, nil
}

func (p *reflectProbe) ran() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.turns)
}

const reflectingCompany = `
name: Acme
roles:
  - name: Engineer
    handle: swe
`

// holderOn is a node on q's broker holding a reflect dispatcher over one probe.
func holderOn(t *testing.T, q *memory.Queue) (*Engine, *reflectProbe) {
	t.Helper()
	c := companyFor(t, reflectingCompany)
	probe := &reflectProbe{}
	r, err := learning.NewReflector(c.Org, q, []learning.Worker{probe}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{backends: &Backends{Queue: q}}
	e.reflector.Store(r)
	return e, probe
}

func reflectTelemetry(runID string) turnTelemetry {
	return turnTelemetry{handle: "swe", role: "Engineer", runID: runID, agentID: "agent-swe"}
}

func reflectedTurn(runID string) types.TurnCompleted {
	return types.TurnCompleted{
		Agent: "agent-swe", AgentHandle: "swe", RoleName: "Engineer", TurnID: runID,
		ToolSequence: []string{"search"}, ReviewOutcome: "done", Outcome: "delivered",
	}
}

func attached(q *memory.Queue, handle string) bool {
	return slices.Contains(q.Attachments(),
		[2]string{topics.AgentReflect(handle), topics.AgentReflectGroup(handle)})
}

// A SEAT'S REFLECTION RUNS ON THE NODE THAT HOLDS IT, and on whichever node holds
// it next once it moves — never on a node a fleet-wide group happened to hand
// it to, where the memory it wrote was never carried or read.
//
// The node that ran a turn puts it on the seat's own reflection subject; the
// holder attaches that subject with the seat and lets it go on release; a wake
// published while nobody holds the seat waits on the subject for the next
// holder, which hydrated the seat's memory before attaching.
func TestASeatsReflectionRunsWhereTheSeatIsHeld(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	holder, probe := holderOn(t, q)

	// THROUGH THE SEAT'S OWN HOOKS: acquisition attaches, release lets go.
	if err := holder.prepareSeat(ctx, "swe", 1, "node-a/1"); err != nil {
		t.Fatalf("prepare the seat: %v", err)
	}
	if !attached(q, "swe") {
		t.Fatal("the holder did not attach the seat's reflection subject")
	}
	holder.publishReflectionDue(ctx, reflectedTurn("run-1"), reflectTelemetry("run-1"))
	if got := probe.ran(); !slices.Equal(got, []string{"run-1"}) {
		t.Fatalf("the holder reflected on %v, want [run-1]", got)
	}

	// A WAKE, never the turn a second time: every event is published once.
	for _, ev := range q.History() {
		if ev.Type == (types.TurnCompleted{}).EventType() {
			t.Errorf("publishing the wake published turn_completed again: %+v", ev)
		}
	}

	// RELEASED, the seat's wakes wait for whoever holds it next.
	holder.releaseSeat(ctx, "swe")
	if attached(q, "swe") {
		t.Fatal("a released seat's reflection subject is still attached here")
	}
	holder.publishReflectionDue(ctx, reflectedTurn("run-2"), reflectTelemetry("run-2"))
	if got := probe.ran(); len(got) != 1 {
		t.Fatalf("the node that let the seat go reflected on %v", got)
	}

	peer := q.Client()
	if err := peer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Stop(context.Background()) })
	next, nextProbe := holderOn(t, peer)
	if err := next.prepareSeat(ctx, "swe", 2, "node-b/1"); err != nil {
		t.Fatalf("prepare the seat on the next holder: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Equal(nextProbe.ran(), []string{"run-2"}) {
		if time.Now().After(deadline) {
			t.Fatalf("the next holder reflected on %v, want the wake that waited for it", nextProbe.ran())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// THE FIRST COMPANY BUILDS THE DISPATCHER, AND A LATER APPLY KEEPS IT: its
// redelivery ring is process state, and a dispatcher rebuilt per apply would
// reflect twice on a wake redelivered across the change.
func TestTheFirstApplyBuildsTheDispatcherAndLaterOnesKeepIt(t *testing.T) {
	t.Parallel()
	e, _ := engineOn(t)
	c := companyFor(t, reflectingCompany)
	if e.reflector.Load() != nil {
		t.Fatal("a node with no company has a dispatcher")
	}
	if err := e.reconfigureReflection(t.Context(), c); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first := e.reflector.Load()
	if first == nil {
		t.Fatal("the first company built no dispatcher")
	}
	if err := e.reconfigureReflection(t.Context(), c); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if e.reflector.Load() != first {
		t.Error("a later apply rebuilt the dispatcher, emptying its redelivery ring")
	}
}
