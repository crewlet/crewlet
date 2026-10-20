package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// presenceEngine is the slice of an engine its presence wiring reads: its id,
// its broker and the sealed channel built from the fleet keyring.
func presenceEngine(t *testing.T, broker *memory.Broker, id string) *Engine {
	t.Helper()
	q := broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	wire, err := newPresenceWire(statelog.OneKey("k1", "the engine presence suite's key material"))
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{id: id, backends: &Backends{Queue: q}, presenceWire: wire}
}

// EVERY NODE WITH A BROKER RUNS A PRESENCE HUB, sealed under the fleet keyring
// and named by the node's own id, and a seat's tool call on one node puts the
// seat in a room a socket holds on another: the hubs the engine arms are the
// ones that tell each other.
func TestEveryNodeRunsAPresenceHubTheFleetHears(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	seats, api := presenceEngine(t, broker, "node-seats"), presenceEngine(t, broker, "node-api")
	for _, e := range []*Engine{seats, api} {
		if err := e.armPresence(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { e.stopPresence(context.Background()) })
		if e.Presence() == nil {
			t.Fatal("a node with a broker armed no presence hub")
		}
	}
	if got := api.PresenceDomain(); got != presence.Channel {
		t.Fatalf("presence frames are sealed in %q, want %q", got, presence.Channel)
	}

	room := presence.ArtifactRoom(artifacts.IDFor("engine-presence-suite"))
	shown := make(chan presence.Member, 16)
	api.Presence().OnChange(func(c presence.Change) {
		for _, m := range c.Upsert {
			shown <- m
		}
	})
	if _, _, err := api.Presence().Join(room, presence.Local{Seat: "dana", Kind: iam.ActorHuman}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		// TOUCHED AGAIN EACH ROUND: the API node's listen is its
		// reconciler's, and a touch before it holds the listen is heard
		// by nobody until the rebroadcast.
		seats.Presence().Touch(room, presence.Agent{Seat: "agent-cto", Turn: "run-1",
			Activity: presence.Editing, Focus: presence.Focus{Collection: "review"}})
		select {
		case m := <-shown:
			if m.Seat == "agent-cto" && m.Kind == iam.ActorAgent {
				return
			}
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatal("the seats node's agent never appeared in the API node's room")
		}
	}
}

// A NODE WITH NO BROKER RUNS NO HUB, and a node with no router hands its
// datastore tools nothing to act through.
func TestANodeWithNoBrokerRunsNoPresence(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	if err := e.armPresence(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.Presence() != nil {
		t.Fatal("a node with no broker armed a presence hub")
	}
	if deps := ArtifactDataToolDeps(e); deps.Store != nil || deps.Turns != nil {
		t.Fatalf("a node with no router was handed datastore tool deps %+v", deps)
	}
}
