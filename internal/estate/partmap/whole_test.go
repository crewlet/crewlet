package partmap

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WITH NO MAP, LAYOUT 0'S ONE PARTITION IS SERVED BY EVERY LIVE DATA NODE, and
// the placement says nothing else: a partition layout 0 does not have is
// refused by name, a roster that cannot say makes the placement say it cannot,
// a node that did not answer (or a refresh) has the roster list again, and a
// placement that is not layout 0's — or has nobody to name its servers, the
// zero value included — refuses rather than answering "nobody serves it".
func TestWithNoMapEveryLiveDataNodeServesTheOnePartition(t *testing.T) {
	t.Parallel()
	r := &roster{nodes: []string{"c", "a", "b"}}
	w := Whole{Running: layoutZero, Roster: r}

	layout, err := w.Layout()
	if err != nil || layout.Number != 0 || len(layout.Partitions()) != 1 {
		t.Fatalf("Layout = (%+v, %v), want layout 0", layout, err)
	}
	nodes, epoch, err := w.Serving(estateZero)
	if err != nil || epoch != 0 || !slices.Equal(nodes, []string{"a", "b", "c"}) {
		t.Fatalf("Serving = (%v, %d, %v), want [a b c] at epoch 0", nodes, epoch, err)
	}
	if _, _, err := w.Serving(statelog.PartitionID{Space: statelog.SpaceTracker}); !errors.Is(err, ErrUnknownPartition) {
		t.Errorf("a partition layout 0 does not have = %v, want ErrUnknownPartition", err)
	}

	w.Unanswered("a")
	if err := w.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	r.mu.Lock()
	invalidated := r.invalidated
	r.err = fmt.Errorf("%w: the presence view is older than a lease survives", coord.ErrUnavailable)
	r.mu.Unlock()
	if invalidated != 2 {
		t.Errorf("a silent node and a refresh invalidated the roster %d time(s), want 2", invalidated)
	}
	if nodes, _, err := w.Serving(estateZero); !errors.Is(err, coord.ErrUnavailable) || nodes != nil {
		t.Errorf("a roster that cannot say reads as (%v, %v), want unknown", nodes, err)
	}

	if _, err := (Whole{Running: smallLayout, Roster: r}).Layout(); !errors.Is(err, ErrNoMap) {
		t.Errorf("a partitioned layout with no map = %v, want ErrNoMap", err)
	}
	for name, zero := range map[string]Whole{
		"the zero value": {}, "no roster": {Running: layoutZero},
	} {
		if _, err := zero.Layout(); err == nil {
			t.Errorf("%s answered a layout", name)
		}
		if nodes, _, err := zero.Serving(estateZero); err == nil {
			t.Errorf("%s answered servers %v rather than refusing", name, nodes)
		}
	}
}

// WITH NO MAP, WHO SERVES LAYOUT 0'S PARTITION IS THE ROUTER'S RULE: a live data
// node whose estate lease says its copy serves or is catching up — never one
// that says `faulted`, which answers `not_holder`, and never one the router
// would not ask because its presence is gone. Nobody serving is unserved; a
// fleet whose leases name no estate at all has nothing to serve and is not.
func TestWithNoMapTheOnePartitionIsServedByTheCopiesARouterWouldAsk(t *testing.T) {
	t.Parallel()
	lease := func(node string, state PartitionState) Presence {
		return Presence{Node: node, Meta: Meta{Weight: 1,
			Partitions: map[string]PartitionState{estateZero.String(): state}}}
	}
	live := []Presence{lease("a", PartFaulted), lease("b", PartCatchingUp),
		lease("c", PartServing), lease("d", PartServing)}

	c, named := WholeCoverage(estateZero, live, []string{"a", "b", "c"})
	if !named || !slices.Equal(c.Serving, []string{"b", "c"}) || c.Wanted != 1 || c.Unserved() {
		t.Fatalf("coverage = (%+v, %v), want b and c serving — a faulted, d's presence gone", c, named)
	}
	c, named = WholeCoverage(estateZero, live[:1], []string{"a", "b", "c"})
	if !named || !c.Unserved() {
		t.Fatalf("every copy faulted reads (%+v, %v), want unserved", c, named)
	}
	c, named = WholeCoverage(estateZero, live[2:], []string{"a", "b"})
	if !named || !c.Unserved() {
		t.Fatalf("every serving copy on a node without presence reads (%+v, %v), want unserved", c, named)
	}
	if _, named := WholeCoverage(estateZero, []Presence{{Node: "a",
		Meta: Meta{Partitions: map[string]PartitionState{}}}}, []string{"a"}); named {
		t.Fatal("a fleet whose leases name no estate read as one with a partition to serve")
	}
}
