package partmap

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The consumers' seams, as the contract states them: the router's placement
// and the join and leave executor's map. Declared here rather than imported
// because each is declared by its consumer, in a package that does not exist
// yet on this build — so a View that stopped satisfying either fails here,
// before the step that consumes it.
var (
	_ interface {
		Layout() (statelog.Layout, error)
		Serving(p statelog.PartitionID) ([]string, uint64, error)
	} = (*View)(nil)
	_ interface {
		Watch(ctx context.Context) (<-chan Map, error)
		Read(ctx context.Context) (Map, uint64, error)
		Fresh() bool
	} = (*View)(nil)
)

// clock is a settable clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// viewOver is a view over this store and these leases, running layout, on
// this clock, with a quick lease heartbeat so the tests need not wait for one.
func viewOver(t *testing.T, maps MapSource, leases coord.Lister, running statelog.Layout, c *clock) *View {
	t.Helper()
	opts := ViewOptions{Maps: maps, Leases: leases, Running: running,
		Heartbeat: 20 * time.Millisecond, TTL: time.Minute}
	if c != nil {
		opts.Now = c.Now
	}
	v, err := NewView(opts)
	if err != nil {
		t.Fatalf("NewView: %v", err)
	}
	return v
}

// run runs a view until the test ends.
func run(t *testing.T, v *View) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = v.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// eventually polls cond until it holds, failing the test after five seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// claimEstate claims node's estate lease saying meta.
func claimEstate(t *testing.T, b coord.Backend, node string, meta Meta) {
	t.Helper()
	raw, err := meta.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.TryAcquire(t.Context(), coord.EstateResource(node), coord.AcquireOptions{
		Owner: node + ":boot", TTL: time.Hour, Ungated: true, Meta: raw,
	}); err != nil {
		t.Fatal(err)
	}
}

// estateLease is a lease at layout, healthy or not, holding estate.000 as said.
func estateLease(layout int, healthy bool, state PartitionState) Meta {
	h := healthy
	return Meta{Weight: 1, Layout: &layout, Healthy: &h,
		Partitions: map[string]PartitionState{"estate.000": state}}
}

var estateZero = statelog.PartitionID{Space: statelog.SpaceEstate}

// UNDER LAYOUT 0 EVERY DATA NODE SERVES THE WHOLE ESTATE, and the view says so:
// a store with no map is layout 0 to a node that runs it, and the one
// partition's servers are the live estate leases that run layout 0, say their
// store is healthy and serve it — at map epoch 0, since there is no map. A copy
// still catching up, a store that says it failed and a node on another layout
// serve nothing; a partition layout 0 does not have is refused by name; and a
// fresh read of the map says there is none.
func TestUnderLayoutZeroEveryDataNodeServesTheWholeEstate(t *testing.T) {
	t.Parallel()
	leases := coordmemory.New()
	claimEstate(t, leases, "a", estateLease(0, true, PartServing))
	claimEstate(t, leases, "b", estateLease(0, true, PartCatchingUp))
	claimEstate(t, leases, "c", estateLease(0, false, PartServing))
	claimEstate(t, leases, "d", estateLease(1, true, PartServing))
	claimEstate(t, leases, "e", estateLease(0, true, PartServing))
	v := viewOver(t, coordmemory.NewFleet(), leases, layoutZero, nil)
	run(t, v)
	eventually(t, "the view to read the store and the leases", v.Fresh)

	layout, err := v.Layout()
	if err != nil || layout.Number != 0 || len(layout.Partitions()) != 1 {
		t.Fatalf("Layout = (%+v, %v), want layout 0", layout, err)
	}
	nodes, epoch, err := v.Serving(estateZero)
	if err != nil || epoch != 0 || !slices.Equal(nodes, []string{"a", "e"}) {
		t.Fatalf("Serving(estate.000) = (%v, %d, %v), want [a e] at epoch 0", nodes, epoch, err)
	}
	if yes, err := v.Serves("a", estateZero); err != nil || !yes {
		t.Errorf("Serves(a) = (%v, %v)", yes, err)
	}
	if yes, err := v.Serves("b", estateZero); err != nil || yes {
		t.Errorf("a copy still catching up serves: (%v, %v)", yes, err)
	}
	if _, _, err := v.Serving(statelog.PartitionID{Space: statelog.SpaceTracker}); !errors.Is(err, ErrUnknownPartition) {
		t.Errorf("a partition layout 0 does not have = %v, want ErrUnknownPartition", err)
	}
	if _, _, err := v.Read(t.Context()); !errors.Is(err, ErrNoMap) {
		t.Errorf("Read = %v, want ErrNoMap", err)
	}
}

// A VIEW THAT CANNOT SAY SAYS SO, as an error wrapping coord.ErrUnavailable:
// never an absent map, a layout or an empty holder list standing in for a store
// it has not heard from — and a node running a partitioned layout with no map is
// told there is none, rather than being handed layout 0.
func TestAViewThatCannotSaySaysSo(t *testing.T) {
	t.Parallel()
	v := viewOver(t, coordmemory.NewFleet(), coordmemory.New(), layoutZero, nil)
	if _, _, _, err := v.Map(); !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("Map before a read = %v, want ErrUnavailable", err)
	}
	if _, err := v.Layout(); !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("Layout before a read = %v, want ErrUnavailable", err)
	}
	if _, _, err := v.Serving(estateZero); !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("Serving before a read = %v, want ErrUnavailable", err)
	}
	if v.Fresh() {
		t.Error("a view that has read nothing is fresh")
	}

	partitioned := viewOver(t, coordmemory.NewFleet(), coordmemory.New(), smallLayout, nil)
	run(t, partitioned)
	eventually(t, "the view to read the store", partitioned.Fresh)
	if _, err := partitioned.Layout(); !errors.Is(err, ErrNoMap) {
		t.Errorf("a partitioned node with no map reads its layout as %v, want ErrNoMap", err)
	}
	if _, _, err := partitioned.Serving(estateZero); !errors.Is(err, ErrNoMap) {
		t.Errorf("a partitioned node with no map reads servers as %v, want ErrNoMap", err)
	}
}

// storeWithMap is a store holding a first map over three nodes at smallLayout,
// and that map.
func storeWithMap(t *testing.T) (*coordmemory.Fleet, MapState, uint64) {
	t.Helper()
	s := newSim(t, smallLayout, 3, "a", "b", "c")
	s.tick()
	raw, err := s.state.Encode()
	if err != nil {
		t.Fatal(err)
	}
	store := coordmemory.NewFleet()
	rec, ok, err := store.CreateEstateMap(t.Context(), raw)
	if err != nil || !ok {
		t.Fatalf("create: %v %v", ok, err)
	}
	return store, s.state, rec.Version
}

// A MAP THAT EXISTS IS THE FLEET'S LAYOUT AND ITS ROUTING, followed as it
// changes: the view takes a new version as it lands — well inside the interval
// it confirms on — a watcher is handed the map it holds and then the new one,
// and a partition's servers are the map's serving holders at its epoch,
// whatever layout this node's own build runs.
func TestAViewFollowsTheMapAsItChanges(t *testing.T) {
	t.Parallel()
	store, state, version := storeWithMap(t)
	v := viewOver(t, store, coordmemory.New(), layoutZero, nil)
	run(t, v)
	eventually(t, "the view to hold the map", func() bool {
		_, got, found, err := v.Map()
		return err == nil && found && got == version
	})
	if layout, err := v.Layout(); err != nil || layout.Number != smallLayout.Number {
		t.Fatalf("Layout = (%d, %v), want the map's, %d", layout.Number, err, smallLayout.Number)
	}
	watch, err := v.Watch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first := <-watch; first.Generation != state.Map.Generation || first.Epoch != state.Map.Epoch {
		t.Fatalf("the watch was handed epoch %d first, want the map held, epoch %d",
			first.Epoch, state.Map.Epoch)
	}

	// THE MAINTAINER'S NEXT TICK, with every joiner saying it serves.
	s := &sim{t: t, state: state, nodes: map[string]*simNode{}, layout: smallLayout,
		company: company(3, "", 1), now: base, served: map[string]bool{}}
	for _, node := range []string{"a", "b", "c"} {
		n := s.add(node, 1, nil)
		for _, p := range state.Map.Partitions {
			n.meta.Partitions[p.ID] = PartServing
		}
		n.meta.MapGeneration, n.meta.MapEpoch = state.Map.Generation, state.Map.Epoch
	}
	s.check = func() {}
	s.tick()
	raw, err := s.state.Encode()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	rec, ok, err := store.UpdateEstateMap(t.Context(), raw, version)
	if err != nil || !ok {
		t.Fatalf("update: %v %v", ok, err)
	}
	next := <-watch
	if next.Epoch != s.state.Map.Epoch {
		t.Fatalf("the watch was handed epoch %d, want %d", next.Epoch, s.state.Map.Epoch)
	}
	if took := time.Since(started); took >= ViewConfirm {
		t.Errorf("the view took %v to see a change: it waited for its confirmation read", took)
	}
	_, held, _, _ := v.Map()
	if held != rec.Version {
		t.Errorf("the view holds version %d, want %d", held, rec.Version)
	}
	p := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 2}
	nodes, epoch, err := v.Serving(p)
	if err != nil || epoch != s.state.Map.Epoch || !slices.Equal(nodes, s.state.Map.Serving(p)) ||
		len(nodes) == 0 {
		t.Errorf("Serving(%s) = (%v, %d, %v), want the map's %v at %d", p, nodes, epoch, err,
			s.state.Map.Serving(p), s.state.Map.Epoch)
	}
}

// A VIEW NEVER TAKES AN OLDER MAP OVER A NEWER ONE: a version a replica served
// late, a read answered before the first map was written that arrives after the
// watch delivered it, and a version this build cannot read — which makes the
// view unknown until a readable one replaces it, rather than leaving it routing
// by the version before.
func TestAViewNeverTakesAnOlderMap(t *testing.T) {
	t.Parallel()
	_, state, _ := storeWithMap(t)
	raw, err := state.Encode()
	if err != nil {
		t.Fatal(err)
	}
	older := state.Clone()
	older.Map.Epoch = 1
	for i := range older.Map.Partitions {
		older.Map.Partitions[i].Holders = nil
	}
	oldRaw, err := older.Encode()
	if err != nil {
		t.Fatal(err)
	}
	v := viewOver(t, coordmemory.NewFleet(), coordmemory.New(), layoutZero, nil)

	before := v.seenVersion()
	v.fold(coord.EstateMapRecord{Value: raw, Version: 5}, true, 0)
	v.fold(coord.EstateMapRecord{}, false, before) // a read asked before version 5
	v.fold(coord.EstateMapRecord{Value: oldRaw, Version: 4}, true, 0)
	m, version, found, err := v.Map()
	if err != nil || !found || version != 5 || m.Epoch != state.Map.Epoch {
		t.Fatalf("the view holds (version %d, epoch %d, found %v, %v), want version 5",
			version, m.Epoch, found, err)
	}

	unreadable := strings.Replace(string(raw), `"generation":"`, `"generation":"not-a-uuid-`, 1)
	v.fold(coord.EstateMapRecord{Value: []byte(unreadable), Version: 6}, true, 0)
	if _, _, _, err := v.Map(); !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("a newest version this build cannot read = %v, want unknown", err)
	}
	if _, _, err := v.Serving(estateZero); !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("routing by the version before an unreadable one: %v", err)
	}
	v.fold(coord.EstateMapRecord{Value: raw, Version: 7}, true, 0)
	if _, version, found, err := v.Map(); err != nil || !found || version != 7 {
		t.Errorf("a readable version after it = (%d, %v, %v), want version 7", version, found, err)
	}

	// AND AN ABSENCE THE STORE ANSWERS WITH NOTHING TAKEN IN MEANWHILE is
	// the map gone, never kept as the last one held.
	v.fold(coord.EstateMapRecord{}, false, v.seenVersion())
	if _, _, found, err := v.Map(); err != nil || found {
		t.Errorf("an answered absence reads as (found %v, %v)", found, err)
	}
}

// failingLister lists the estate leases until it is told to stop answering.
type failingLister struct {
	coord.Lister
	mu     sync.Mutex
	broken bool
}

func (f *failingLister) ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error) {
	f.mu.Lock()
	broken := f.broken
	f.mu.Unlock()
	if broken {
		return nil, errors.New("coordination timed out")
	}
	return f.Lister.ListLive(ctx, class)
}

// A VIEW IS FRESH ONLY WHILE BOTH HALVES WERE CONFIRMED WITHIN THE STALENESS
// BOUND, statelog.FloorCacheStale: the map by a read or a delivery the store
// answered, the leases by a listing. Past it, the one that went quiet makes the
// whole view unknown to anything that decides from it — and ROUTING still
// answers from what it holds, since a wrong route costs a retry.
func TestAViewIsFreshOnlyWhileBothHalvesAreConfirmed(t *testing.T) {
	t.Parallel()
	c := &clock{now: base}
	leases := &failingLister{Lister: coordmemory.New()}
	store, _, _ := storeWithMap(t)
	v := viewOver(t, store, leases, layoutZero, c)
	run(t, v)
	eventually(t, "a fresh view", v.Fresh)

	// THE MAP GOES QUIET: no read confirms it within the bound, while the
	// leases go on being listed.
	c.advance(statelog.FloorCacheStale + time.Second)
	eventually(t, "a listing at the new instant", func() bool {
		_, listed, err := v.leases.Leases()
		return err == nil && !listed.Before(c.Now())
	})
	if v.Fresh() {
		t.Fatal("a view whose map was last confirmed past the bound is fresh")
	}
	if _, _, err := v.Serving(statelog.PartitionID{Space: statelog.SpaceTracker}); err != nil {
		t.Errorf("routing refused a view that is only old: %v", err)
	}
	if _, _, err := v.Read(t.Context()); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !v.Fresh() {
		t.Fatal("a view whose map was just read and whose leases were just listed is not fresh")
	}

	// THE LEASES GO QUIET: every listing fails, and the map is confirmed.
	leases.mu.Lock()
	leases.broken = true
	leases.mu.Unlock()
	c.advance(statelog.FloorCacheStale + time.Second)
	if _, _, err := v.Read(t.Context()); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.Fresh() {
		t.Error("a view whose leases were last listed past the bound is fresh")
	}
}

// A WATCH ENDS WITH ITS CONTEXT, and hands nothing to a fleet with no map.
func TestAViewWatchEndsWithItsContext(t *testing.T) {
	t.Parallel()
	v := viewOver(t, coordmemory.NewFleet(), coordmemory.New(), layoutZero, nil)
	run(t, v)
	ctx, cancel := context.WithCancel(t.Context())
	watch, err := v.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case m, ok := <-watch:
		t.Fatalf("a fleet with no map was handed %+v (open %v)", m, ok)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case _, ok := <-watch:
		if ok {
			t.Fatal("the watch delivered after its context ended")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not close when its context ended")
	}
}
