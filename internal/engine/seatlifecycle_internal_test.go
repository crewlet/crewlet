package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A SEAT'S ARRIVAL IS KEYED BY ITS AGENT ID, the one field the live
// projection reads its key from on every seat-level event. It carried the
// HANDLE on `agent_spawned` and `agent_terminated` alone, so a seat's arrival
// and its first turn landed under two identities — and a seat renamed after
// it was created arrived under a key none of its own turns carried.
func TestASeatsLifecycleEventNamesTheSeatByItsAgentID(t *testing.T) {
	t.Parallel()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })

	got := make(chan types.AgentSpawned, 4)
	if err := q.Subscribe(t.Context(), topics.Event("agent_spawned"), "probe",
		func(_ context.Context, ev *events.Event) queue.Result {
			if p, ok := events.DataAs[*types.AgentSpawned](ev); ok && p != nil {
				got <- *p
			}
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Two seats sharing a name, and the second one RENAMED: it answers to
	// `eng-b` now and was created as `builder`, which is what its id is
	// derived from.
	a := &org.Role{Name: "Engineer", DeclaredHandle: "eng-a"}
	b := &org.Role{Name: "Engineer", DeclaredHandle: "eng-b", OriginHandle: "builder",
		FormerHandles: []string{"builder"}}
	company := &Company{
		Config: &config.Company{},
		Org:    &org.Organization{Name: "Nimbus", Roles: []*org.Role{a, b}},
	}
	e := &Engine{backends: &Backends{Queue: q}}
	e.epoch.current.Store(company)

	idA, _ := company.Org.AgentIDFor(a)
	idB, _ := company.Org.AgentIDFor(b)
	if idA == idB {
		t.Fatalf("the fixture's two seats derive one id %s", idA)
	}

	e.publishSeatLifecycle(t.Context(), "eng-b", types.AgentSpawned{})
	select {
	case p := <-got:
		if p.Agent != idB.String() {
			t.Errorf("agent_id = %q, want the renamed seat's derived id %s — "+
				"not its handle, and not its namesake's id %s", p.Agent, idB, idA)
		}
		if p.AgentHandle != "eng-b" || p.RoleName != "Engineer" {
			t.Errorf("the event names handle %q and role %q, want the seat's current ones",
				p.AgentHandle, p.RoleName)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no agent_spawned was published")
	}

	// A handle this company does not hold publishes nothing: there is no
	// seat to key the row on, and an identity made up from the handle
	// would be a second row for one seat.
	e.publishSeatLifecycle(t.Context(), "nobody", types.AgentSpawned{})
	select {
	case p := <-got:
		t.Errorf("a handle nobody holds published %+v", p)
	case <-time.After(200 * time.Millisecond):
	}
}
