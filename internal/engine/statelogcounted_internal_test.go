package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE SNAPSHOT LOOP COUNTS WHAT THE TRIM COUNTS.
//
// Whether there is anybody to donate an artefact to is the trim's own counted
// set: every node whose row names a log, every live data node — one that has
// published no row yet is exactly a joiner waiting for the copy this count
// decides whether anybody takes — less the tombstones past their window. So a
// joiner with no row counts, a live node holding no data does not, and an
// evicted node's row stops counting once its eviction is past the window, as
// it does in the trim.
//
// JUDGED BY NAME, AND WITHOUT THIS NODE. Whether the running node counts
// itself yet rests on two loops that run on their own clocks after it admits
// seats — its first positions row, and its presence view's next listing (the
// view starts listing before the node claims its presence, so its first
// listing can name nobody) — and neither is what this test is about. Taken as
// a baseline SIZE that later counts add to, it failed under load: the node
// was not yet counted when the baseline was read, its own row then satisfied
// the wait for the joiner, and the joiner arriving later read as an evicted
// row still counted past its window.
func TestTheSnapshotLoopCountsWhatTheTrimCounts(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.core.Load().log
	ctx := t.Context()
	now := time.Now()
	// others is every node counted at the instant other than this one.
	others := func(at time.Time) []string {
		t.Helper()
		n, err := e.counted(ctx, s, at)
		if err != nil {
			t.Fatalf("counted: %v", err)
		}
		return slices.DeleteFunc(n, func(id string) bool { return id == s.nodeID })
	}

	// The stateless node is claimed FIRST, so any listing that names the
	// joiner began after both claims and names the stateless node too: the
	// moment the joiner is counted is a moment the stateless node could have
	// been, and the check below it is not vacuous.
	for _, n := range []struct {
		id    string
		roles []string
	}{
		{"node-seats", []string{"seats"}},
		{"node-joiner", []string{"data", "seats"}},
	} {
		if _, _, err := e.backends.Coord.TryAcquire(ctx, coord.NodeResource(n.id), coord.AcquireOptions{
			Owner: n.id + ":1", TTL: time.Hour, Meta: map[string]any{"roles": n.roles},
		}); err != nil {
			t.Fatalf("claim %s's presence: %v", n.id, err)
		}
	}
	e.dataView.Invalidate()
	waitUntil(t, 10*time.Second, "the joiner to be counted", func() bool {
		return slices.Contains(others(now), "node-joiner")
	})
	if got, want := others(now), []string{"node-joiner"}; !slices.Equal(got, want) {
		t.Fatalf("with a joiner and a stateless node live the fleet counts %v "+
			"beside this node, want %v", got, want)
	}

	if err := s.fleet.PutPositions(ctx, coord.NodePositions{
		NodeID: "node-gone", At: now.UTC(),
		Domains: map[string]coord.DomainPosition{worklet.Domain{}.Name(): {Seq: 1}},
	}); err != nil {
		t.Fatalf("publish node-gone's row: %v", err)
	}
	if got, want := others(now), []string{"node-gone", "node-joiner"}; !slices.Equal(got, want) {
		t.Fatalf("with node-gone's row the fleet counts %v beside this node, want %v", got, want)
	}
	mustApply(t, "evict node-gone", func() (worklet.WriteResult, error) {
		return e.native.Load().writer.EvictNode(ctx, statelog.NewOpID(now, "evict-node-gone"), "node-gone")
	})
	if got, want := others(now), []string{"node-gone", "node-joiner"}; !slices.Equal(got, want) {
		t.Errorf("inside the fence window the fleet counts %v beside this node, "+
			"want node-gone still counted (%v), as the trim counts it", got, want)
	}
	past := now.Add(statelog.EvictionFenceWindow + time.Minute)
	if got, want := others(past), []string{"node-joiner"}; !slices.Equal(got, want) {
		t.Errorf("past the fence window the fleet counts %v beside this node, "+
			"want node-gone's row uncounted (%v), as the trim uncounts it", got, want)
	}
}

// A PRESENCE VIEW THAT CANNOT ANSWER COUNTS THE REGISTER'S HALF, and fails
// nothing: the count decides whether a copy is worth taking, and a
// coordination blip that says nothing about this node's disk must not stamp a
// failed take on its row.
func TestAnUnansweredPresenceViewCountsTheRegisterAlone(t *testing.T) {
	t.Parallel()
	view, err := coord.NewLeaseView(failingLister{}, coord.ClassNode,
		coord.ViewOptions{Every: time.Hour, Trust: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = view.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })

	fleet := newCountedFleet(t, "node-a", "node-b")
	e := &Engine{dataView: view}
	s := &stateLog{fleet: fleet}
	n, err := e.counted(t.Context(), s, time.Now())
	if err != nil {
		t.Fatalf("an unanswered presence view failed the count: %v", err)
	}
	if len(n) != 2 {
		t.Errorf("counted %v, want the register's two rows", n)
	}
}

// failingLister is a coordination store whose every listing and watch fails.
type failingLister struct{}

func (failingLister) ListLive(context.Context, coord.Class) ([]coord.Lease, error) {
	return nil, errors.New("coordination timed out")
}

func (failingLister) WatchLive(context.Context, coord.Class) (<-chan coord.LeaseWrite, error) {
	return nil, errors.New("coordination timed out")
}

// newCountedFleet is a fleet whose positions register holds a row for each node,
// naming the tracker's log.
func newCountedFleet(t *testing.T, nodes ...string) coord.Fleet {
	t.Helper()
	fleet := coordmem.NewFleet()
	for _, id := range nodes {
		if err := fleet.PutPositions(t.Context(), coord.NodePositions{
			NodeID: id, At: time.Now().UTC(),
			Domains: map[string]coord.DomainPosition{worklet.Domain{}.Name(): {Seq: 1}},
		}); err != nil {
			t.Fatalf("publish %s's row: %v", id, err)
		}
	}
	return fleet
}
