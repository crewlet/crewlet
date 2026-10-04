package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
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
func TestTheSnapshotLoopCountsWhatTheTrimCounts(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.native.Load().log
	ctx := t.Context()
	now := time.Now()
	count := func(at time.Time) int {
		t.Helper()
		n, err := e.countedOn(ctx, s, statelog.EstatePartition, at)
		if err != nil {
			t.Fatalf("countedOn: %v", err)
		}
		return len(n)
	}
	before := count(now)

	for id, roles := range map[string][]string{
		"node-joiner": {"data", "seats"},
		"node-seats":  {"seats"},
	} {
		if _, _, err := e.backends.Coord.TryAcquire(ctx, coord.NodeResource(id), coord.AcquireOptions{
			Owner: id + ":1", TTL: time.Hour, Meta: map[string]any{"roles": roles},
		}); err != nil {
			t.Fatalf("claim %s's presence: %v", id, err)
		}
	}
	e.dataView.Invalidate()
	waitUntil(t, 10*time.Second, "the joiner to be counted", func() bool {
		return count(now) == before+1
	})

	if err := s.fleet.PutPositions(ctx, coord.NodePositions{
		NodeID: "node-gone", At: now.UTC(),
		Domains: map[string]coord.DomainPosition{tracker.Domain{}.Name(): {Seq: 1}},
	}); err != nil {
		t.Fatalf("publish node-gone's row: %v", err)
	}
	if got := count(now); got != before+2 {
		t.Fatalf("with node-gone's row the fleet counts %d, want %d", got, before+2)
	}
	mustApply(t, "evict node-gone", func() (tracker.WriteResult, error) {
		return e.native.Load().writer.EvictNode(ctx, statelog.NewOpID(now, "evict-node-gone"), "node-gone")
	})
	if got := count(now); got != before+2 {
		t.Errorf("inside the fence window the fleet counts %d, want node-gone still "+
			"counted (%d), as the trim counts it", got, before+2)
	}
	if got := count(now.Add(statelog.EvictionFenceWindow + time.Minute)); got != before+1 {
		t.Errorf("past the fence window the fleet counts %d, want node-gone's row "+
			"uncounted (%d), as the trim uncounts it", got, before+1)
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
	s := &stateLog{layout: LayoutZero(), fleet: fleet}
	n, err := e.countedOn(t.Context(), s, statelog.EstatePartition, time.Now())
	if err != nil {
		t.Fatalf("an unanswered presence view failed the count: %v", err)
	}
	if len(n) != 2 {
		t.Errorf("counted %v, want the register's two rows", n)
	}
}

// failingLister is a coordination store whose every listing fails.
type failingLister struct{}

func (failingLister) ListLive(context.Context, coord.Class) ([]coord.Lease, error) {
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
			Domains: map[string]coord.DomainPosition{tracker.Domain{}.Name(): {Seq: 1}},
		}); err != nil {
			t.Fatalf("publish %s's row: %v", id, err)
		}
	}
	return fleet
}
