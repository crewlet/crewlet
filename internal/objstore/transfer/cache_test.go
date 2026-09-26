package transfer

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/placement"
)

type storedMap struct {
	rec   coord.ObjectMapRecord
	found bool
}

func (s *storedMap) ObjectMap(context.Context) (coord.ObjectMapRecord, bool, error) {
	return s.rec, s.found, nil
}

func encoded(t *testing.T, epoch uint64) []byte {
	t.Helper()
	raw, err := objstore.MapState{Map: placement.Map{Epoch: epoch, Replicas: 1,
		Members: []placement.Member{{Node: "data-a", Weight: 1}}}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A MAP THIS BUILD CANNOT READ LEAVES THE ONE IT HAD IN PLACE — placing by the
// last map it understood is correct until it upgrades, where placing by
// nothing stops every upload on the node.
func TestAnUnreadableMapKeepsThePreviousOne(t *testing.T) {
	t.Parallel()
	store := &storedMap{}
	c := NewCache(store)
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Current(); ok {
		t.Fatal("a fleet with no map reads as having one")
	}
	store.rec, store.found = coord.ObjectMapRecord{Value: encoded(t, 3), Version: 1}, true
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	store.rec = coord.ObjectMapRecord{Value: []byte(`{"map":{"replicas":0}}`), Version: 2}
	if err := c.Refresh(t.Context()); err == nil {
		t.Fatal("an invalid map refreshed cleanly")
	}
	if m, ok := c.Current(); !ok || m.Epoch != 3 {
		t.Fatalf("after an unreadable map the cache holds %+v, %v; want epoch 3", m, ok)
	}
}

// A SLOW READ NEVER ROLLS BACK A FAST ONE: the maintainer installs its own
// write at once, and a refresh that read the store a moment before it lands
// must not put the older map back.
func TestAnOlderMapNeverReplacesANewerOne(t *testing.T) {
	t.Parallel()
	c := NewCache(&storedMap{})
	newer, err := objstore.DecodeMapState(encoded(t, 5))
	if err != nil {
		t.Fatal(err)
	}
	older, err := objstore.DecodeMapState(encoded(t, 4))
	if err != nil {
		t.Fatal(err)
	}
	c.Observe(newer, 10)
	c.Observe(older, 9)
	if m, _ := c.Current(); m.Epoch != 5 {
		t.Fatalf("the cache holds epoch %d after an older read, want 5", m.Epoch)
	}
}
