package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY LOG COUNTS THE LIVE DATA NODES, AND ONE THAT HAS NOT REPORTED COUNTS AT
// ZERO.
//
// The trim may not remove a record a node that applies the log has not
// applied, and every data node applies every log from boot. So a data node
// still joining — no position yet — counts at zero on every log and blocks it:
// the tail it is about to replay is exactly what the trim must keep.
func TestEveryLogCountsTheLiveDataNodes(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.native.Load().log
	e.stopRetention()
	s.publishPositions(t.Context())
	r := &retention{fleet: e.backends.Fleet, state: s, nodeID: e.id,
		holders: fixedHolders{{NodeID: e.id}, {NodeID: "node-joining"}}}
	shared, err := r.read(t.Context())
	if err != nil {
		t.Fatalf("read the tick's inputs: %v", err)
	}
	if len(s.running()) == 0 {
		t.Fatal("the premise: the node runs no log")
	}
	for _, running := range s.running() {
		var ids []string
		for _, n := range shared.counted(running, nil) {
			ids = append(ids, n.NodeID)
			if n.NodeID == "node-joining" && (n.Seq != 0 || n.Generation != 0) {
				t.Errorf("%s counts the joining data node at %d@%d, want zero — it "+
					"has reported nothing, and the tail it is about to replay is what "+
					"the trim must keep", running.key, n.Seq, n.Generation)
			}
		}
		if want := slices.Sorted(slices.Values([]string{e.id, "node-joining"})); !slices.Equal(ids, want) {
			t.Errorf("%s counts %v, want every live data node %v", running.key, ids, want)
		}
	}
}

// THE HOLDERS ARE THE LIVE DATA NODES, LISTED AFRESH: the estate is held whole
// by every data node and by nothing else, so the counted set's live half is the
// presence the trim has always counted — data nodes only, since a node without
// `data` applies no log — the same nodes the router asks.
func TestTheHoldersAreTheLiveDataNodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := coordmem.New()
	claimPresences(t, backend, map[string][]string{
		"data-a": {"data", "seats"}, "agent-1": {"seats"},
	})
	e := &Engine{backends: &Backends{Coord: backend}}
	holders := e.holdersOf()
	if _, byPresence := holders.(presenceHolders); !byPresence {
		t.Fatalf("the holders are answered by %T, want a fresh presence listing", holders)
	}
	live, err := holders.LiveData(ctx)
	if err != nil {
		t.Fatalf("LiveData: %v", err)
	}
	if len(live) != 1 || live[0].NodeID != "data-a" {
		t.Errorf("the estate is held by %v, want the one live data node", live)
	}
}

// claimPresences claims a presence lease for every node, with its roles.
func claimPresences(t *testing.T, backend coord.Backend, roles map[string][]string) {
	t.Helper()
	for id, r := range roles {
		if _, _, err := backend.TryAcquire(t.Context(), coord.NodeResource(id), coord.AcquireOptions{
			Owner: id + ":1", TTL: time.Hour, Meta: map[string]any{"roles": r},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// fixedHolders answers the live data nodes from a list a case wrote.
type fixedHolders []statelog.Presence

func (f fixedHolders) LiveData(context.Context) ([]statelog.Presence, error) {
	return slices.Clone(f), nil
}
