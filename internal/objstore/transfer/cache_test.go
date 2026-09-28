package transfer

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/objstore"
)

type storedMap struct {
	mu    sync.Mutex
	rec   coord.ObjectMapRecord
	found bool
}

func (s *storedMap) ObjectMap(context.Context) (coord.ObjectMapRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec, s.found, nil
}

func (s *storedMap) set(rec coord.ObjectMapRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec, s.found = rec, true
}

// generation is one lineage of maps a test writes.
var generation = uuid.MustParse("6f1c6d2e-35f4-4d0e-9d55-0c6c3c1b2a01")

func state(t *testing.T, gen uuid.UUID, epoch uint64, nodes ...string) objstore.MapState {
	t.Helper()
	m := testMap(1, nodes...)
	m.Generation, m.Epoch = gen, epoch
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	return objstore.MapState{Map: m}
}

func encoded(t *testing.T, gen uuid.UUID, epoch uint64) []byte {
	t.Helper()
	raw, err := state(t, gen, epoch, "data-a").Encode()
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
	if _, ok := c.Layout(); ok {
		t.Fatal("a fleet with no map has a layout")
	}
	store.set(coord.ObjectMapRecord{Value: encoded(t, generation, 3), Version: 1})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	store.set(coord.ObjectMapRecord{Value: []byte(`{"map":{"replicas":0}}`), Version: 2})
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
	c.Observe(state(t, generation, 5, "data-a"), 10)
	c.Observe(state(t, generation, 4, "data-a"), 9)
	if m, _ := c.Current(); m.Epoch != 5 {
		t.Fatalf("the cache holds epoch %d after an older read, want 5", m.Epoch)
	}
}

// A MAP OF ANOTHER GENERATION REPLACES THE ONE HELD WHATEVER ITS VERSION: the
// key was lost and written again, its versions started again with it, and a
// cache comparing them would keep placing by the lost map until the new one's
// version overtook it — for ever, if the old one had been rewritten often.
func TestARecreatedMapIsTakenWhateverItsVersion(t *testing.T) {
	t.Parallel()
	c := NewCache(&storedMap{})
	c.Observe(state(t, generation, 40, "data-a"), 900)
	again := uuid.MustParse("0d4f0c5a-9f0e-4c55-8a0b-9b1a4a2c7e02")
	c.Observe(state(t, again, 1, "data-a", "data-b"), 1)
	m, _ := c.Current()
	if m.Generation != again || m.Epoch != 1 {
		t.Fatalf("after the map was recreated the cache holds %s epoch %d, want the new one",
			m.Generation, m.Epoch)
	}
	l, _ := c.Layout()
	if l.Map().Generation != again {
		t.Fatal("the layout is still the lost map's")
	}
}

// THE LAYOUT IS COMPUTED ONCE PER MAP and answers for that map: a record whose
// map is unchanged — an absence counted, a measurement taken — keeps the
// layout it had, and a map that changed gets its own.
func TestTheLayoutFollowsTheMapAndOnlyTheMap(t *testing.T) {
	t.Parallel()
	c := NewCache(&storedMap{})
	first := state(t, generation, 2, "data-a", "data-b", "data-c")
	c.Observe(first, 1)
	l1, ok := c.Layout()
	if !ok || l1.Map().Epoch != 2 {
		t.Fatalf("layout = %v, %v", l1, ok)
	}
	for pg := range first.Map.Groups() {
		if !slices.Equal(l1.Up(pg), first.Map.Ranked(pg)[:first.Map.Size()]) {
			t.Fatalf("group %d's layout is %v, the map ranks %v", pg, l1.Up(pg), first.Map.Ranked(pg))
		}
	}

	counted := first.Clone()
	counted.Absence = map[string]membership.Absence{"data-c": {Ticks: 1, Reason: membership.ReasonAbsent}}
	c.Observe(counted, 2)
	if l2, _ := c.Layout(); l2 != l1 {
		t.Fatal("a record whose map did not change had its layout computed again")
	}
	if s, v, _ := c.State(); v != 2 || s.Absence["data-c"].Ticks != 1 {
		t.Fatalf("the record was not installed: version %d, %+v", v, s.Absence)
	}

	moved := state(t, generation, 3, "data-a", "data-b")
	c.Observe(moved, 3)
	l3, _ := c.Layout()
	if l3 == l1 || l3.Map().Epoch != 3 || len(l3.Map().Members) != 2 {
		t.Fatalf("a changed map kept the old layout: epoch %d, %d members", l3.Map().Epoch,
			len(l3.Map().Members))
	}
}

// blockingMap answers its first read only once released, with a map of one
// generation, and every later read at once with a map of another.
type blockingMap struct {
	reads   atomic.Int32
	release chan struct{}
	first   coord.ObjectMapRecord
	later   coord.ObjectMapRecord
}

func (b *blockingMap) ObjectMap(context.Context) (coord.ObjectMapRecord, bool, error) {
	if b.reads.Add(1) == 1 {
		<-b.release
		return b.first, true, nil
	}
	return b.later, true, nil
}

// TWO REFRESHES NEVER INSTALL OUT OF ORDER. A refresh that read the store
// before the map was recreated must not install its lost map over one a later
// refresh read — and since a different generation replaces whatever is held,
// only the read and its install happening together, one refresh at a time,
// guarantees that.
func TestRefreshesInstallInTheOrderTheyRead(t *testing.T) {
	t.Parallel()
	again := uuid.MustParse("0d4f0c5a-9f0e-4c55-8a0b-9b1a4a2c7e03")
	store := &blockingMap{
		release: make(chan struct{}),
		first:   coord.ObjectMapRecord{Value: encoded(t, generation, 9), Version: 90},
		later:   coord.ObjectMapRecord{Value: encoded(t, again, 1), Version: 1},
	}
	c := NewCache(store)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.Refresh(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	for store.reads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.Refresh(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	// The second refresh has had every chance to read and install ahead of
	// the first, which it may only do if nothing orders them.
	time.Sleep(50 * time.Millisecond)
	close(store.release)
	wg.Wait()
	if m, _ := c.Current(); m.Generation != again {
		t.Fatalf("the cache holds generation %s — a read from before the map was recreated "+
			"was installed over one from after", m.Generation)
	}
}
