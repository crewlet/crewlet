package engine

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

	room := artifacts.IDFor("engine-presence-suite")
	shown := make(chan artifacts.PresenceMember, 16)
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
		seats.Presence().Touch(room, "agent-cto", "run-1", artifacts.PresenceEditing,
			artifacts.Focus{Collection: "review"})
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

// THE ARTIFACT ROOMS FOLLOW THE ARTIFACT COPIES' OWN VERDICT, and no other
// domain's.
//
// The rooms withhold both artifact appliers' pushes while this node's copy of
// either log is out of service, and tell every room to re-read at each switch.
// The watch read the TRACKER's verdict — the inbox relay's question — so a
// wrong artifacts or datastore copy went on pushing what it applied to every
// room, while a wrong tracker copy, which describes nothing a room shows,
// withheld every room's sound pushes. Each step here holds one verdict for a
// few rechecks on the bubble's clock and reads what the rooms were told.
//
// Mutation: watch the tracker's copy again, or either artifact log alone, and
// a step hears the wrong switch.
func TestTheArtifactRoomsFollowTheArtifactCopiesOwnVerdict(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var verdict atomic.Pointer[copyVerdict]
		set := func(v copyVerdict) { verdict.Store(&v) }
		set(soundCopy())
		e := &Engine{}
		withCore(e)
		e.local = localWith(e, time.Now, func(context.Context, *core) copyVerdict {
			return *verdict.Load()
		})
		var mu sync.Mutex
		var told []bool
		e.SetOnArtifactsServing(func(serving bool) {
			mu.Lock()
			told = append(told, serving)
			mu.Unlock()
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			e.watchArtifactCopies(ctx)
		}()
		defer func() { cancel(); <-done }()

		for _, step := range []struct {
			name string
			now  copyVerdict
			want []bool
		}{
			{"a wrong tracker copy", wrongOn(trackerName), nil},
			{"a wrong artifacts copy", wrongOn(artifactCopies[0]), []bool{false}},
			{"both copies back", soundCopy(), []bool{false, true}},
			{"a wrong datastore copy", wrongOn(artifactCopies[1]), []bool{false, true, false}},
		} {
			set(step.now)
			time.Sleep(3 * servingRecheck)
			synctest.Wait()
			mu.Lock()
			got := slices.Clone(told)
			mu.Unlock()
			if !slices.Equal(got, step.want) {
				t.Fatalf("after %s the rooms were told %v, want %v", step.name, got, step.want)
			}
		}
	})
}
