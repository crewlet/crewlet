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
