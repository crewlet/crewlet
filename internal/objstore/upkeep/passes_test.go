package upkeep

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/disk"
	"github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/objstore/transfer"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// refs is a domain whose references a test sets directly.
type refs struct {
	mu         sync.Mutex
	set        map[objstore.Hash]struct{}
	incomplete bool
	barrierErr error
	barriers   int
}

func (r *refs) Name() string { return "test" }

func (r *refs) Barrier(context.Context) (statelog.Position, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.barriers++
	return statelog.Position{Stream: "TEST", Seq: uint64(r.barriers)}, r.barrierErr
}

func (r *refs) Referenced(_ context.Context, pg int, _ statelog.Position) (map[objstore.Hash]struct{}, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[objstore.Hash]struct{}{}
	for h := range r.set {
		if h.PG() == pg {
			out[h] = struct{}{}
		}
	}
	return out, !r.incomplete, nil
}

func (r *refs) refer(hs ...objstore.Hash) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range hs {
		r.set[h] = struct{}{}
	}
}

// dataNode is one member: its disk, the map IT places by, its client and its
// passes.
type dataNode struct {
	name   string
	disk   *disk.Store
	m      atomic.Pointer[placement.Map]
	client *transfer.Client
	passes *Node
	now    time.Time
}

func (d *dataNode) maps() (placement.Map, bool) {
	m := d.m.Load()
	if m == nil {
		return placement.Map{}, false
	}
	return *m, true
}

type fleet struct {
	nodes map[string]*dataNode
	refs  *refs
	m     placement.Map
}

// newFleet is members on one broker, each placing by the same map.
func newFleet(t *testing.T, replicas int, names ...string) *fleet {
	t.Helper()
	broker := memory.NewBroker()
	start := func() *memory.Queue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
		return q
	}
	f := &fleet{nodes: map[string]*dataNode{}, refs: &refs{set: map[objstore.Hash]struct{}{}}}
	f.m = placement.Map{Epoch: 3, Replicas: replicas}
	for _, name := range slices.Sorted(slices.Values(names)) {
		f.m.Members = append(f.m.Members, placement.Member{Node: name, Weight: 1})
	}
	for _, name := range names {
		store, err := disk.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		d := &dataNode{name: name, disk: store, now: time.Now()}
		m := f.m
		d.m.Store(&m)
		stop, err := transfer.Serve(t.Context(), start(), name, store, d.maps)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stop(context.Background()) })
		d.client, err = transfer.NewClient(transfer.ClientOptions{Queue: start(), Self: name,
			Local: store, Maps: d.maps})
		if err != nil {
			t.Fatal(err)
		}
		d.passes, err = NewNode(NodeOptions{Self: name, Local: store, Peers: d.client,
			Maps: d.maps, References: References{f.refs},
			Now: func() time.Time { return d.now }})
		if err != nil {
			t.Fatal(err)
		}
		f.nodes[name] = d
	}
	return f
}

// chunkPlaced is a chunk whose up set is exactly the named members.
func (f *fleet) chunkPlaced(t *testing.T, on ...string) ([]byte, objstore.Hash) {
	t.Helper()
	want := slices.Sorted(slices.Values(on))
	for i := range 100_000 {
		data := []byte{byte(i), byte(i >> 8), byte(i >> 16), 'c'}
		h := objstore.HashOf(data)
		if slices.Equal(slices.Sorted(slices.Values(f.m.Up(h.PG()))), want) {
			return data, h
		}
	}
	t.Fatalf("no chunk is placed on %v", on)
	return nil, ""
}

// A NODE FETCHES WHAT THE MAP PLACES ON IT AND IT DOES NOT HOLD, and counts
// what no member can supply as missing rather than failed.
func TestRepairFetchesWhatThisNodeShouldHold(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2, "data-a", "data-b", "data-c")
	data, h := f.chunkPlaced(t, "data-a", "data-b")
	if err := f.nodes["data-b"].disk.Put(h, data); err != nil {
		t.Fatal(err)
	}
	_, lost := f.chunkPlaced(t, "data-a", "data-c")
	f.refs.refer(h, lost)

	a := f.nodes["data-a"]
	if err := a.passes.Repair(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !a.disk.Has(h) {
		t.Fatal("repair did not copy the chunk the map places here")
	}
	st := a.passes.Status()
	if st.Fetched != 1 || st.Missing != 1 || st.Placed != 2 || st.Epoch != 3 || st.Repaired.IsZero() {
		t.Fatalf("status = %+v, want 1 fetched, 1 missing, 2 placed at epoch 3", st)
	}

	// AND ONLY WHAT IS PLACED HERE: data-c is not in this chunk's up set.
	c := f.nodes["data-c"]
	if err := c.passes.Repair(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c.disk.Has(h) {
		t.Fatal("repair copied a chunk to a node the map does not place it on")
	}
}

// A CHUNK NOTHING REFERS TO IS DELETED ONCE IT IS PAST ITS GRACE, and not a
// moment before — bytes are uploaded before the record naming them lands.
func TestAnUnreferencedChunkIsCollectedAfterItsGrace(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 1, "data-a")
	a := f.nodes["data-a"]
	data := []byte("uploaded, never recorded")
	h := objstore.HashOf(data)
	if err := a.disk.Put(h, data); err != nil {
		t.Fatal(err)
	}
	if err := a.passes.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !a.disk.Has(h) {
		t.Fatal("a chunk inside its grace was collected")
	}
	a.now = time.Now().Add(PendingGrace + time.Minute)
	if err := a.passes.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a.disk.Has(h) {
		t.Fatal("an unreferenced chunk past its grace survived")
	}
	if st := a.passes.Status(); st.Deleted != 1 {
		t.Fatalf("status = %+v, want 1 deleted", st)
	}
}

// A REFERENCED CHUNK IS NEVER COLLECTED, however old.
func TestAReferencedChunkIsKeptHoweverOld(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 1, "data-a")
	a := f.nodes["data-a"]
	data := []byte("the file somebody attached")
	h := objstore.HashOf(data)
	if err := a.disk.Put(h, data); err != nil {
		t.Fatal(err)
	}
	f.refs.refer(h)
	a.now = time.Now().Add(365 * 24 * time.Hour)
	if err := a.passes.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !a.disk.Has(h) {
		t.Fatal("a referenced chunk was collected")
	}
}

// NOTHING IS COLLECTED ON A VIEW THAT IS NOT CURRENT OR NOT COMPLETE: a record
// this node has not applied, or could not, may be the one naming the chunk.
func TestNothingIsCollectedOnAViewThatIsBehindOrIncomplete(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 1, "data-a")
	a := f.nodes["data-a"]
	data := []byte("named by a record this node cannot read")
	h := objstore.HashOf(data)
	if err := a.disk.Put(h, data); err != nil {
		t.Fatal(err)
	}
	a.now = time.Now().Add(PendingGrace + time.Minute)

	f.refs.barrierErr = errors.New("the log did not answer")
	if err := a.passes.Collect(t.Context()); err == nil {
		t.Fatal("a pass whose barrier failed reported success")
	}
	if !a.disk.Has(h) {
		t.Fatal("a pass that could not bring its view current collected a chunk")
	}

	f.refs.barrierErr = nil
	f.refs.incomplete = true
	if err := a.passes.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !a.disk.Has(h) {
		t.Fatal("a pass over an incomplete view collected a chunk")
	}
	if st := a.passes.Status(); st.Skipped == "" {
		t.Fatalf("status = %+v, want the reason nothing was collected", st)
	}
}

// A NODE WITH NO MAP DELETES NOTHING: it cannot say what it should hold, and
// "nothing" is the one answer that would empty it.
func TestANodeWithNoMapDeletesNothing(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 1, "data-a")
	a := f.nodes["data-a"]
	a.m.Store(nil)
	data := []byte("kept")
	h := objstore.HashOf(data)
	if err := a.disk.Put(h, data); err != nil {
		t.Fatal(err)
	}
	a.now = time.Now().Add(PendingGrace * 10)
	if err := a.passes.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !a.disk.Has(h) {
		t.Fatal("a node with no map collected a chunk")
	}
}

// A COPY BEYOND THIS NODE'S PLACEMENT IS DELETED WHEN EVERY MEMBER PLACING IT
// HOLDS IT AND SAYS SO AT THE SAME EPOCH — and kept on any other answer.
func TestACopyBeyondThePlacementIsDroppedOnlyOnceConfirmed(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2, "data-a", "data-b", "data-c")
	data, h := f.chunkPlaced(t, "data-a", "data-b")
	f.refs.refer(h)
	c := f.nodes["data-c"]
	if err := c.disk.Put(h, data); err != nil {
		t.Fatal(err)
	}
	if err := f.nodes["data-a"].disk.Put(h, data); err != nil {
		t.Fatal(err)
	}

	// ONE PLACING MEMBER DOES NOT HOLD IT YET: kept.
	if err := c.passes.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !c.disk.Has(h) {
		t.Fatal("a copy was dropped while a placing member lacked the chunk")
	}

	// BOTH HOLD IT, BUT ONE PLACES BY ANOTHER EPOCH: kept.
	if err := f.nodes["data-b"].disk.Put(h, data); err != nil {
		t.Fatal(err)
	}
	older := f.m
	older.Epoch--
	f.nodes["data-b"].m.Store(&older)
	if err := c.passes.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !c.disk.Has(h) {
		t.Fatal("a copy was dropped on the word of a member placing by another map")
	}

	// ALL CONFIRM AT ONE EPOCH: dropped.
	same := f.m
	f.nodes["data-b"].m.Store(&same)
	if err := c.passes.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c.disk.Has(h) {
		t.Fatal("a confirmed copy beyond the placement survived")
	}
	if st := c.passes.Status(); st.Strays != 1 {
		t.Fatalf("status = %+v, want 1 stray dropped", st)
	}
	for _, n := range []string{"data-a", "data-b"} {
		if !f.nodes[n].disk.Has(h) {
			t.Fatalf("%s lost its placed copy", n)
		}
	}
}

// answering is a peer whose answer about every chunk a test fixes.
type answering struct {
	held, placed bool
	epoch        uint64
	err          error
}

func (a answering) Fetch(context.Context, objstore.Hash) ([]byte, error) {
	return nil, transfer.ErrNotFound
}

func (a answering) Has(_ context.Context, node string, hs []objstore.Hash) (transfer.Holding, error) {
	out := transfer.Holding{Node: node, Epoch: a.epoch}
	for range hs {
		out.Held = append(out.Held, a.held)
		out.Placed = append(out.Placed, a.placed)
	}
	return out, a.err
}

// THE CONFIRMATION RULE, answer by answer. Only "I hold it, my own map places
// it here, and my map is yours" lets a copy go: a member that merely HOLDS a
// chunk may be holding a copy beyond its own placement, and dropping ours on
// its word is how two nodes reading different maps would each drop theirs.
func TestOnlyAFullConfirmationDropsACopy(t *testing.T) {
	t.Parallel()
	m := placement.Map{Epoch: 3, Replicas: 1, Members: []placement.Member{{Node: "data-a", Weight: 1}}}
	for _, c := range []struct {
		name    string
		peer    answering
		dropped bool
	}{
		{"held, placed, same epoch", answering{held: true, placed: true, epoch: 3}, true},
		{"held but not placed there", answering{held: true, placed: false, epoch: 3}, false},
		{"placed but not held", answering{held: false, placed: true, epoch: 3}, false},
		{"another epoch", answering{held: true, placed: true, epoch: 4}, false},
		{"no answer", answering{err: transfer.ErrNoAnswer}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, err := disk.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			data := []byte("a copy beyond the placement")
			h := objstore.HashOf(data)
			if err := store.Put(h, data); err != nil {
				t.Fatal(err)
			}
			n, err := NewNode(NodeOptions{Self: "data-z", Local: store, Peers: c.peer,
				Maps:       func() (placement.Map, bool) { return m, true },
				References: References{&refs{set: map[objstore.Hash]struct{}{h: {}}}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := n.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			if dropped := !store.Has(h); dropped != c.dropped {
				t.Fatalf("dropped = %v, want %v", dropped, c.dropped)
			}
		})
	}
}

// A NODE WITH NO SOURCE OF REFERENCES IS REFUSED: it would read every chunk as
// unreferenced and delete the lot a day later.
func TestPassesWithNoSourceAreRefused(t *testing.T) {
	t.Parallel()
	store, err := disk.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = NewNode(NodeOptions{Self: "data-a", Local: store, Peers: &transfer.Client{},
		Maps: func() (placement.Map, bool) { return placement.Map{}, false }})
	if err == nil {
		t.Fatal("passes with no references were built")
	}
}
