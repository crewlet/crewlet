package partmap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

func (c *clock) set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

// roster is a settable presence roster: the live data nodes, or why they are
// not known.
type roster struct {
	mu          sync.Mutex
	nodes       []string
	err         error
	invalidated int
}

func (r *roster) LiveDataNodes() ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.nodes), r.err
}

func (r *roster) Invalidate() {
	r.mu.Lock()
	r.invalidated++
	r.mu.Unlock()
}

// viewOver is a view over this store and these leases, running layout, on
// this clock, with a quick lease heartbeat so the tests need not wait for one,
// and a presence roster naming a, b and c.
func viewOver(t *testing.T, maps MapSource, leases coord.Lister, running statelog.Layout, c *clock) *View {
	t.Helper()
	return viewWith(t, maps, leases, running, c, &roster{nodes: []string{"a", "b", "c"}})
}

// viewWith is viewOver with this presence roster.
func viewWith(t *testing.T, maps MapSource, leases coord.Lister, running statelog.Layout, c *clock,
	r Roster) *View {

	t.Helper()
	opts := ViewOptions{Maps: maps, Leases: leases, Running: running, Roster: r,
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

// UNDER LAYOUT 0 EVERY LIVE DATA NODE SERVES THE WHOLE ESTATE, and the view
// says so from PRESENCE: a store with no map is layout 0 to a node that runs
// it, and the one partition's servers are the live data nodes the presence
// roster names, at map epoch 0 — whatever the estate leases say. A data node
// of a build that claims no estate lease is served (a rolling upgrade that
// replaced the stateless nodes first must still reach it), and so is one whose
// copy is catching up; an estate lease nobody's presence backs serves nothing.
// A roster that cannot say makes the view say it cannot; a node that did not
// answer invalidates the roster as well as the leases; a partition layout 0
// does not have is refused by name; and a fresh read of the map says there is
// none.
func TestUnderLayoutZeroEveryLiveDataNodeServesTheWholeEstate(t *testing.T) {
	t.Parallel()
	leases := coordmemory.New()
	claimEstate(t, leases, "a", estateLease(0, true, PartServing))
	claimEstate(t, leases, "b", estateLease(0, true, PartCatchingUp))
	claimEstate(t, leases, "x", estateLease(0, true, PartServing))
	// c runs a build from before the estate lease: presence, and no lease.
	r := &roster{nodes: []string{"c", "a", "b"}}
	v := viewWith(t, coordmemory.NewFleet(), leases, layoutZero, nil, r)
	run(t, v)
	eventually(t, "the view to read the store and the leases", v.Fresh)

	layout, err := v.Layout()
	if err != nil || layout.Number != 0 || len(layout.Partitions()) != 1 {
		t.Fatalf("Layout = (%+v, %v), want layout 0", layout, err)
	}
	nodes, epoch, err := v.Serving(estateZero)
	if err != nil || epoch != 0 || !slices.Equal(nodes, []string{"a", "b", "c"}) {
		t.Fatalf("Serving(estate.000) = (%v, %d, %v), want every live data node [a b c] at "+
			"epoch 0", nodes, epoch, err)
	}
	if yes, err := v.Serves("c", estateZero); err != nil || !yes {
		t.Errorf("a data node that claims no estate lease is not served: (%v, %v)", yes, err)
	}
	if yes, err := v.Serves("x", estateZero); err != nil || yes {
		t.Errorf("an estate lease with no presence behind it serves: (%v, %v)", yes, err)
	}
	if _, _, err := v.Serving(statelog.PartitionID{Space: statelog.SpaceTracker}); !errors.Is(err, ErrUnknownPartition) {
		t.Errorf("a partition layout 0 does not have = %v, want ErrUnknownPartition", err)
	}
	if _, _, err := v.Read(t.Context()); !errors.Is(err, ErrNoMap) {
		t.Errorf("Read = %v, want ErrNoMap", err)
	}

	v.Invalidate()
	r.mu.Lock()
	invalidated := r.invalidated
	r.err = fmt.Errorf("%w: the presence view is older than a lease survives", coord.ErrUnavailable)
	r.mu.Unlock()
	if invalidated != 1 {
		t.Errorf("an unanswered node invalidated the roster %d time(s), want 1", invalidated)
	}
	if nodes, _, err := v.Serving(estateZero); !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("a roster that cannot say reads as (%v, %v), want unknown", nodes, err)
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

	// A NODE RUNNING LAYOUT 0 WITH NOBODY TO NAME ITS SERVERS is refused
	// at construction, rather than answering "nobody" on every request.
	if _, err := NewView(ViewOptions{Maps: coordmemory.NewFleet(), Leases: coordmemory.New(),
		Running: layoutZero, Heartbeat: time.Second, TTL: time.Minute}); err == nil {
		t.Error("a view running layout 0 was built with no roster of live data nodes")
	}

	partitioned := viewWith(t, coordmemory.NewFleet(), coordmemory.New(), smallLayout, nil, nil)
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

// encoded is a state's stored form.
func encoded(t *testing.T, s MapState) []byte {
	t.Helper()
	raw, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// unreadable is a stored map this build cannot decode — its generation is not
// a uuid, so not even its lineage reads.
func unreadable(raw []byte) []byte {
	return []byte(strings.Replace(string(raw), `"generation":"`, `"generation":"not-a-uuid-`, 1))
}

// A VIEW NEVER TAKES AN OLDER MAP OVER A NEWER ONE: a version a replica served
// late, a read answered before the first map was written that arrives after the
// watch delivered it, and a version this build cannot read — which makes the
// view unknown until a readable one replaces it, rather than leaving it routing
// by the version before, or handing that version to a reader who starts
// watching meanwhile.
func TestAViewNeverTakesAnOlderMap(t *testing.T) {
	t.Parallel()
	_, state, _ := storeWithMap(t)
	raw := encoded(t, state)
	older := state.Clone()
	older.Map.Epoch = 1
	for i := range older.Map.Partitions {
		older.Map.Partitions[i].Holders = nil
	}
	oldRaw := encoded(t, older)
	v := viewOver(t, coordmemory.NewFleet(), coordmemory.New(), layoutZero, nil)

	before := v.takenCount()
	v.delivered(coord.EstateMapRecord{Value: raw, Version: 5})
	v.answered(coord.EstateMapRecord{}, false, before, v.now()) // a read asked before version 5
	if _, version, found, err := v.Map(); err != nil || !found || version != 5 {
		t.Fatalf("a read answered before the first map, arriving after it, left (version %d, "+
			"found %v, %v), want version 5", version, found, err)
	}
	v.delivered(coord.EstateMapRecord{Value: oldRaw, Version: 4})
	before = v.takenCount()
	v.delivered(coord.EstateMapRecord{Value: raw, Version: 6})
	v.answered(coord.EstateMapRecord{Value: raw, Version: 5}, true, before, v.now()) // raced version 6
	m, version, found, err := v.Map()
	if err != nil || !found || version != 6 || m.Epoch != state.Map.Epoch {
		t.Fatalf("the view holds (version %d, epoch %d, found %v, %v), want version 6",
			version, m.Epoch, found, err)
	}

	v.delivered(coord.EstateMapRecord{Value: unreadable(raw), Version: 7})
	if _, _, _, err := v.Map(); !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("a newest version this build cannot read = %v, want unknown", err)
	}
	if _, _, err := v.Serving(estateZero); !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("routing by the version before an unreadable one: %v", err)
	}
	watch, err := v.Watch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-watch:
		t.Fatalf("a reader watching while the newest version is unreadable was handed "+
			"epoch %d, a map the fleet has replaced", m.Epoch)
	case <-time.After(50 * time.Millisecond):
	}
	v.delivered(coord.EstateMapRecord{Value: raw, Version: 8})
	if _, version, found, err := v.Map(); err != nil || !found || version != 8 {
		t.Errorf("a readable version after it = (%d, %v, %v), want version 8", version, found, err)
	}
	select {
	case m := <-watch:
		if m.Generation != state.Map.Generation || m.Epoch != state.Map.Epoch {
			t.Errorf("the watch was handed epoch %d first, want the readable version's %d",
				m.Epoch, state.Map.Epoch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the readable version after an unreadable one never reached the watch")
	}

	// AND AN ABSENCE THE STORE ANSWERS WITH NOTHING TAKEN IN MEANWHILE is
	// the map gone, never kept as the last one held.
	v.answered(coord.EstateMapRecord{}, false, v.takenCount(), v.now())
	if _, _, found, err := v.Map(); err != nil || found {
		t.Errorf("an answered absence reads as (found %v, %v)", found, err)
	}
	// ...and a late delivery of the lineage it lost is not taken back in.
	v.delivered(coord.EstateMapRecord{Value: raw, Version: 8})
	if _, _, found, err := v.Map(); err != nil || found {
		t.Errorf("a late delivery of the lost map was taken back in: (found %v, %v)", found, err)
	}
}

// A VIEW TAKES A MAP WRITTEN AGAIN AFTER ITS KEY WAS LOST, whatever its version:
// a recreated key starts its versions again, so a view that ordered by version
// alone would keep routing by the lost map — or keep answering that there is
// none — until the new one's version overtook the old, while every read of the
// store confirmed it. A new lineage replaces the old whether the watch or a read
// hands it over, and whether the view held the old map or had seen it go; a
// read that nothing overtook is the store's value now, so a store restored to
// an EARLIER version of the same lineage is taken too, and so is a new lineage
// this build cannot read; but a read that raced the new lineage's delivery and
// answered the old one is late, and changes nothing.
func TestAViewTakesAMapWrittenAgainAfterItsKeyWasLost(t *testing.T) {
	t.Parallel()
	_, lost, _ := storeWithMap(t)
	_, recreated, _ := storeWithMap(t)
	if lost.Map.Generation == recreated.Map.Generation {
		t.Fatal("the premise: two first maps name two lineages")
	}
	lostRaw, newRaw := encoded(t, lost), encoded(t, recreated)
	holds := func(v *View, want MapState, version uint64) {
		t.Helper()
		m, got, found, err := v.Map()
		if err != nil || !found || got != version || m.Generation != want.Map.Generation {
			t.Fatalf("the view holds (lineage %s, version %d, found %v, %v), want lineage %s "+
				"at version %d", m.Generation, got, found, err, want.Map.Generation, version)
		}
	}

	// BY THE WATCH, over a map held at a higher version.
	v := viewOver(t, coordmemory.NewFleet(), coordmemory.New(), layoutZero, nil)
	v.delivered(coord.EstateMapRecord{Value: lostRaw, Version: 57})
	watch, err := v.Watch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	<-watch
	v.delivered(coord.EstateMapRecord{Value: newRaw, Version: 3})
	holds(v, recreated, 3)
	select {
	case m := <-watch:
		if m.Generation != recreated.Map.Generation {
			t.Errorf("the watch was handed lineage %s, want the recreated map's", m.Generation)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a map written again never reached the watch")
	}
	// ...and the lost lineage delivered late is not taken back.
	before := v.takenCount()
	v.delivered(coord.EstateMapRecord{Value: newRaw, Version: 4})
	v.answered(coord.EstateMapRecord{Value: lostRaw, Version: 57}, true, before, v.now())
	holds(v, recreated, 4)

	// BY THE WATCH, after a read saw the key gone.
	v = viewOver(t, coordmemory.NewFleet(), coordmemory.New(), layoutZero, nil)
	v.delivered(coord.EstateMapRecord{Value: lostRaw, Version: 57})
	v.answered(coord.EstateMapRecord{}, false, v.takenCount(), v.now())
	v.delivered(coord.EstateMapRecord{Value: newRaw, Version: 1})
	holds(v, recreated, 1)

	// BY A READ NOTHING OVERTOOK, after a read saw the key gone.
	v = viewOver(t, coordmemory.NewFleet(), coordmemory.New(), layoutZero, nil)
	v.delivered(coord.EstateMapRecord{Value: lostRaw, Version: 57})
	v.answered(coord.EstateMapRecord{}, false, v.takenCount(), v.now())
	v.answered(coord.EstateMapRecord{Value: newRaw, Version: 1}, true, v.takenCount(), v.now())
	holds(v, recreated, 1)

	// A STORE RESTORED TO AN EARLIER VERSION OF THE SAME LINEAGE, by a
	// read nothing overtook: the store's value now.
	v = viewOver(t, coordmemory.NewFleet(), coordmemory.New(), layoutZero, nil)
	v.delivered(coord.EstateMapRecord{Value: lostRaw, Version: 57})
	v.answered(coord.EstateMapRecord{Value: lostRaw, Version: 30}, true, v.takenCount(), v.now())
	holds(v, lost, 30)
	v.delivered(coord.EstateMapRecord{Value: lostRaw, Version: 31})
	holds(v, lost, 31)

	// A NEW LINEAGE THIS BUILD CANNOT READ, delivered at a lower version:
	// unknown, never the lost map.
	v = viewOver(t, coordmemory.NewFleet(), coordmemory.New(), layoutZero, nil)
	v.delivered(coord.EstateMapRecord{Value: lostRaw, Version: 57})
	// A holder state this build does not know: the lineage still reads.
	future := []byte(strings.Replace(string(newRaw), `"state":"`, `"state":"resting-`, 1))
	if _, err := DecodeMapState(future); err == nil || bytes.Equal(future, newRaw) {
		t.Fatal("the premise: a map this build cannot decode")
	}
	v.delivered(coord.EstateMapRecord{Value: future, Version: 2})
	if _, _, _, err := v.Map(); !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("an unreadable map of a new lineage left the view answering %v, want unknown", err)
	}
}

// quietWatch is a store whose watch delivers nothing until it ends.
type quietWatch struct{ MapSource }

func (q quietWatch) WatchEstateMap(ctx context.Context) (<-chan coord.EstateMapRecord, error) {
	ch := make(chan coord.EstateMapRecord)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
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
	// A WATCH THAT DELIVERS NOTHING: the map goes quiet by construction,
	// so the only confirmations are the reads this test makes — a first
	// delivery landing after the clock moved would confirm the view at
	// the new instant, which is a delivery doing its job rather than the
	// view going quiet.
	v := viewOver(t, quietWatch{store}, leases, layoutZero, c)
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
	// AND IT SAYS WHICH HALF, HOW OLD AND AGAINST WHAT: what an alarm reports.
	if st := v.Staleness(c.Now()); st.Half != "the estate map" || !st.Stale() ||
		st.Age != statelog.FloorCacheStale+time.Second || st.Bound != statelog.FloorCacheStale {
		t.Errorf("Staleness = %+v, want the estate map a second past the bound", st)
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
	quiet := c.Now()
	c.advance(statelog.FloorCacheStale + time.Second)
	if _, _, err := v.Read(t.Context()); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.Fresh() {
		t.Error("a view whose leases were last listed past the bound is fresh")
	}
	// THE LEASES' LAST LISTING IS STILL AGED past their trust, which is the
	// age an alarm reports — not a zero time a reader cannot measure from.
	if st := v.Staleness(c.Now()); st.Half != "the estate leases" || !st.Stale() ||
		st.Age < c.Now().Sub(quiet) {
		t.Errorf("Staleness = %+v at %v, want the leases, listed before %v", st, c.Now(), quiet)
	}
}

// heldMaps is a store whose reads wait to be let go, saying when each is
// asked.
type heldMaps struct {
	MapSource
	asked   chan struct{}
	release chan struct{}
}

func (h *heldMaps) EstateMap(ctx context.Context) (coord.EstateMapRecord, bool, error) {
	select {
	case h.asked <- struct{}{}:
	case <-ctx.Done():
		return coord.EstateMapRecord{}, false, ctx.Err()
	}
	select {
	case <-h.release:
	case <-ctx.Done():
		return coord.EstateMapRecord{}, false, ctx.Err()
	}
	return h.MapSource.EstateMap(ctx)
}

// confirmedAtOf is when the view last confirmed its map half.
func confirmedAtOf(v *View) time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.confirmedAt
}

// waitFor waits for a signal on ch, failing the test after five seconds.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A READ CONFIRMS THE MAP HALF AS OF WHEN IT WAS ASKED, never when its answer
// arrived — and nothing moves the confirmation back.
//
// What a read answers held at some instant between the two, and only the first
// is one the view can vouch for. Stamped on arrival, a read asked just before
// the store stopped answering confirmed the view at an instant after it had:
// the view read fresh, and its estate_view_stale alarm stayed quiet, for a
// store that had not answered since — which is how the engine's own case for a
// stale view failed whenever the view's first read answered after that case
// moved its clock.
func TestAReadConfirmsTheViewWhenItWasAsked(t *testing.T) {
	t.Parallel()
	c := &clock{now: base}
	store, state, version := storeWithMap(t)
	held := &heldMaps{MapSource: quietWatch{store}, asked: make(chan struct{}, 1),
		release: make(chan struct{})}
	v := viewOver(t, held, coordmemory.New(), layoutZero, c)

	answered := make(chan error, 1)
	go func() {
		_, _, err := v.Read(t.Context())
		answered <- err
	}()
	<-held.asked
	c.advance(time.Minute)
	close(held.release)
	if err := <-answered; err != nil {
		t.Fatalf("Read: %v", err)
	}
	if at := confirmedAtOf(v); !at.Equal(base) {
		t.Fatalf("a read asked at %v and answered a minute later confirmed the view at %v — "+
			"an instant the store was never asked at", base, at)
	}

	// A DELIVERY IS STAMPED WHEN IT ARRIVES, and a read asked before it that
	// answers after it says nothing newer.
	v.delivered(coord.EstateMapRecord{Value: encoded(t, state), Version: version})
	if at := confirmedAtOf(v); !at.Equal(c.Now()) {
		t.Fatalf("a delivery confirmed the view at %v, want %v", at, c.Now())
	}
	v.answered(coord.EstateMapRecord{Value: encoded(t, state), Version: version}, true,
		v.takenCount(), base)
	if at := confirmedAtOf(v); !at.Equal(c.Now()) {
		t.Errorf("a read asked before a delivery, answering after it, moved the "+
			"confirmation back to %v from %v", at, c.Now())
	}
}

// THE VIEW'S OWN CONFIRMATION READ IS DATED WHEN IT WAS ASKED, as a caller's
// Read is: the read a running view makes every ViewConfirm, which is what keeps
// a healthy fleet's view fresh and what the engine's case for a stale view
// caught confirming the view after its clock had moved. The two take the
// instant in one place; this holds the running view to it, which a case
// driving Read alone cannot — the loop's read dated on arrival passed every
// other case here.
func TestTheViewsOwnReadIsDatedWhenItWasAsked(t *testing.T) {
	t.Parallel()
	c := &clock{now: base}
	store, _, _ := storeWithMap(t)
	held := &heldMaps{MapSource: quietWatch{store}, asked: make(chan struct{}, 1),
		release: make(chan struct{})}
	v := viewOver(t, held, coordmemory.New(), layoutZero, c)
	run(t, v)

	waitFor(t, held.asked, "the view to read the map")
	c.advance(time.Minute)
	close(held.release)
	eventually(t, "the view's read to answer", func() bool { return !confirmedAtOf(v).IsZero() })
	if at := confirmedAtOf(v); !at.Equal(base) {
		t.Fatalf("the view's own read, asked at %v and answered a minute later, confirmed "+
			"the view at %v — an instant the store was never asked at", base, at)
	}
}

// THE LEASES ARE UNKNOWN AT THEIR TTL WHERE THAT IS SHORTER THAN THE STALENESS
// BOUND, and the view's own staleness says so at the same instant: a listing
// fifty seconds old under a forty-five-second TTL is no answer at all, so the
// view is not fresh — and Staleness, which the estate_view_stale alarm reads,
// calls the leases stale then too, rather than at the minute every other
// cached fact is held to, which left a gap where nothing could decide from the
// view and nothing said why. Under a TTL longer than the bound, the bound is
// the minute.
func TestAViewsLeasesAreStaleAtTheirTTLWhereThatIsShorter(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		ttl, bound time.Duration
	}{
		"the default TTL":        {ttl: 45 * time.Second, bound: 45 * time.Second},
		"a TTL past the minute":  {ttl: 5 * time.Minute, bound: statelog.FloorCacheStale},
		"a TTL at the bound":     {ttl: statelog.FloorCacheStale, bound: statelog.FloorCacheStale},
		"a TTL well short of it": {ttl: 9 * time.Second, bound: 9 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &clock{now: base}
			leases := &failingLister{Lister: coordmemory.New()}
			store, _, _ := storeWithMap(t)
			v, err := NewView(ViewOptions{Maps: quietWatch{store}, Leases: leases,
				Running: layoutZero, Roster: &roster{nodes: []string{"a"}},
				Heartbeat: 20 * time.Millisecond, TTL: tc.ttl, Now: c.Now})
			if err != nil {
				t.Fatal(err)
			}
			run(t, v)
			eventually(t, "a fresh view", v.Fresh)
			leases.mu.Lock()
			leases.broken = true
			leases.mu.Unlock()

			// THE MAP IS NOT READ AGAIN, so both halves are as old as each
			// other and the one the view names is the one further past its
			// own bound — the leases, wherever their TTL is the shorter.
			for _, age := range []time.Duration{tc.bound, tc.bound + time.Second} {
				c.set(base.Add(age))
				st := v.Staleness(c.Now())
				stale := age > tc.bound
				if st.Stale() != stale || v.Fresh() == stale {
					t.Errorf("leases last listed %v ago under a %v TTL: Staleness %+v, Fresh %v; "+
						"want both to say stale %v", age, tc.ttl, st, v.Fresh(), stale)
				}
				if stale && st.Bound != tc.bound {
					t.Errorf("the stale half reads %+v, want it judged against %v", st, tc.bound)
				}
				if stale && tc.bound < statelog.FloorCacheStale && st.Half != "the estate leases" {
					t.Errorf("the stale half reads %+v, want the estate leases", st)
				}
			}
		})
	}
}

// THE LEASES ARE LISTED AT LEAST EVERY ViewConfirm, whatever their heartbeat:
// a listing more often than they renew finds nothing new but confirms, and a
// view listed only on a long TTL's heartbeat went stale between every two
// listings on a healthy fleet.
func TestAViewListsTheLeasesAtLeastEveryConfirmation(t *testing.T) {
	t.Parallel()
	for heartbeat, want := range map[time.Duration]time.Duration{
		3 * time.Second:              3 * time.Second,
		ViewConfirm:                  ViewConfirm,
		100 * time.Second:            ViewConfirm,
		statelog.FloorCacheStale * 2: ViewConfirm,
	} {
		if got := viewListEvery(heartbeat); got != want {
			t.Errorf("a heartbeat of %v lists every %v, want %v", heartbeat, got, want)
		}
	}

	// AND THE VIEW LISTS ON IT: an hour's heartbeat, and a second listing
	// within a confirmation — real time, since the lease view's cadence is
	// a ticker.
	listings := &countingLister{Lister: coordmemory.New()}
	v, err := NewView(ViewOptions{Maps: coordmemory.NewFleet(), Leases: listings,
		Running: layoutZero, Roster: &roster{nodes: []string{"a"}},
		Heartbeat: time.Hour, TTL: 3 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	run(t, v)
	deadline := time.Now().Add(ViewConfirm + 10*time.Second)
	for listings.count() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the leases were listed %d time(s) in %v under an hour's heartbeat, "+
				"want again within %v", listings.count(), ViewConfirm+10*time.Second, ViewConfirm)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// countingLister counts the listings a view makes.
type countingLister struct {
	coord.Lister
	mu sync.Mutex
	n  int
}

func (c *countingLister) ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.Lister.ListLive(ctx, class)
}

func (c *countingLister) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// THE LEASES A VIEW LISTED ARE WHAT IT HANDS AN ALARM, each read as the
// maintainer reads one — and a listing it does not have is unknown, never an
// empty fleet.
func TestAViewHandsOverTheLeasesItListed(t *testing.T) {
	t.Parallel()
	leases := &failingLister{Lister: coordmemory.New()}
	for _, node := range []string{"a", "b"} {
		claimEstate(t, leases.Lister.(coord.Backend), node, estateLease(0, true, PartServing))
	}
	v := viewOver(t, coordmemory.NewFleet(), leases, layoutZero, nil)
	if _, err := v.Presences(); !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("a view that has not listed hands over (%v), want unknown", err)
	}
	run(t, v)
	eventually(t, "the view to list the leases", v.Fresh)
	got, err := v.Presences()
	if err != nil || len(got) != 2 || got[0].Meta.Healthy == nil {
		t.Fatalf("Presences = (%+v, %v), want a and b, read as leases", got, err)
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
