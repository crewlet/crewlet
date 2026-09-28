package upkeep

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/disk"
	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/objstore/transfer"
	"github.com/crewlet/crewlet/internal/placement"
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

	// readAt is every position a read was asked to be no earlier than.
	readAt []statelog.Position
}

func newRefs(hs ...objstore.Hash) *refs {
	r := &refs{set: map[objstore.Hash]struct{}{}}
	r.refer(hs...)
	return r
}

func (r *refs) Name() string { return "test" }

func (r *refs) Barrier(context.Context) (statelog.Position, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.barriers++
	return statelog.Position{Stream: "TEST", Seq: uint64(r.barriers)}, r.barrierErr
}

func (r *refs) Referenced(_ context.Context, lo, hi int, at statelog.Position) (map[objstore.Hash]struct{}, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readAt = append(r.readAt, at)
	out := map[objstore.Hash]struct{}{}
	for h := range r.set {
		if h.Slot() >= lo && h.Slot() < hi {
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

// testMap is a valid map of equal members.
func testMap(bits, replicas int, names ...string) objplacement.Map {
	m := objplacement.Map{Generation: uuid.New(), Epoch: 3, Replicas: replicas, PGBits: bits}
	for _, name := range slices.Sorted(slices.Values(names)) {
		m.Members = append(m.Members, placement.Member{Node: name, Weight: 1,
			Share: placement.DefaultShare(1)})
	}
	return m
}

// dataNode is one member: its disk, the map IT places by, its client and its
// passes.
type dataNode struct {
	name   string
	disk   *disk.Store
	layout atomic.Pointer[objplacement.Layout]
	client *transfer.Client
	passes *Node
	now    atomic.Pointer[time.Time]
}

func (d *dataNode) layouts() (*objplacement.Layout, bool) {
	l := d.layout.Load()
	return l, l != nil
}

func (d *dataNode) place(m objplacement.Map) { d.layout.Store(m.Layout()) }

func (d *dataNode) setNow(t time.Time) { d.now.Store(&t) }

type fleet struct {
	nodes map[string]*dataNode
	refs  *refs
	m     objplacement.Map
	l     *objplacement.Layout
}

// openStore is a data node's chunk store, on a volume pinned half full. Its
// directory is a temporary one on whatever disk runs the test, and a store
// measuring that disk is nearfull on a machine 85% used and refuses every new
// chunk past 95% — so a pass that stores a chunk would fail on the machine's
// free space rather than on anything the test asserts about the pass.
func openStore(t *testing.T) *disk.Store {
	t.Helper()
	store, err := disk.Options{Space: halfFull}.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if h := store.Health(); h.State != disk.HealthOK {
		t.Fatalf("a store on a volume pinned half full = %+v, want ok", h)
	}
	return store
}

// halfFull is a volume of a hundred gibibytes, half of it free.
func halfFull(string) (capacity, free uint64, err error) { return 100 << 30, 50 << 30, nil }

// newFleet is members on one broker, each placing by the same map at the
// fewest group bits.
func newFleet(t *testing.T, replicas int, names ...string) *fleet {
	t.Helper()
	return newFleetAt(t, objplacement.MinPGBits, replicas, names...)
}

// newFleetAt is newFleet at a given group count.
func newFleetAt(t *testing.T, bits, replicas int, names ...string) *fleet {
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
	f := &fleet{nodes: map[string]*dataNode{}, refs: newRefs()}
	f.m = testMap(bits, replicas, names...)
	f.l = f.m.Layout()
	for _, name := range names {
		store := openStore(t)
		d := &dataNode{name: name, disk: store}
		d.setNow(time.Now())
		d.place(f.m)
		stop, err := transfer.Serve(t.Context(), start(), name, store, d.layouts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stop(context.Background()) })
		d.client, err = transfer.NewClient(transfer.ClientOptions{Queue: start(), Self: name,
			Local: store, Layouts: d.layouts})
		if err != nil {
			t.Fatal(err)
		}
		d.passes, err = NewNode(NodeOptions{Self: name, Local: store, Peers: d.client,
			Layouts: d.layouts, References: References{f.refs},
			Now: func() time.Time { return *d.now.Load() }})
		if err != nil {
			t.Fatal(err)
		}
		f.nodes[name] = d
	}
	return f
}

// upOf is the members a chunk's group is placed on.
func (f *fleet) upOf(h objstore.Hash) []string { return f.l.Up(f.l.Group(h.Slot())) }

// chunkPlaced is a chunk whose up set is exactly the named members.
func (f *fleet) chunkPlaced(t *testing.T, on ...string) ([]byte, objstore.Hash) {
	t.Helper()
	want := slices.Sorted(slices.Values(on))
	for i := range 100_000 {
		data := []byte{byte(i), byte(i >> 8), byte(i >> 16), 'c'}
		h := objstore.HashOf(data)
		if slices.Equal(slices.Sorted(slices.Values(f.upOf(h))), want) {
			return data, h
		}
	}
	t.Fatalf("no chunk is placed on %v", on)
	return nil, ""
}

// A NODE FETCHES WHAT THE MAP PLACES ON IT AND IT DOES NOT HOLD, and says what
// it did: how many chunks are placed here, how many it holds, how many it
// fetched, and what it still lacks — a chunk no member holds counted MISSING
// rather than failed, since nothing a retry does can find it.
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
	want := RepairStatus{Epoch: 3, Completed: true, Placed: 2, Held: 1, Pending: 1, Fetched: 1,
		Missing: 1, At: st.Repair.At}
	if st.Repair != want || st.Repair.At.IsZero() {
		t.Fatalf("status = %+v, want %+v", st.Repair, want)
	}
	if st.Repaired != st.Repair {
		t.Fatalf("a complete pass is not the last complete pass: %+v", st.Repaired)
	}
	if got := st.Repair.Report(); got != (RepairReport{Epoch: 3, Pending: 1}) {
		t.Fatalf("report = %+v", got)
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

// A REPAIR PINS THE ESTATE FIRST, on the barrier collection takes, so a node
// back from being away repairs what was written while it was gone — and a pass
// that could not is not complete, leaving the last complete pass as the
// node's word on what it holds.
func TestRepairReadsAnEstatePinnedAtItsStart(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 1, "data-a")
	a := f.nodes["data-a"]
	if err := a.passes.Repair(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.refs.mu.Lock()
	barriers, reads := f.refs.barriers, slices.Clone(f.refs.readAt)
	f.refs.mu.Unlock()
	if barriers != 1 || len(reads) == 0 {
		t.Fatalf("%d barriers and %d reads, want one barrier before the reads", barriers, len(reads))
	}
	for _, at := range reads {
		if at != (statelog.Position{Stream: "TEST", Seq: 1}) {
			t.Fatalf("a read was asked at %+v, not at the barrier the pass took", at)
		}
	}
	complete := a.passes.Status().Repaired

	f.refs.barrierErr = errors.New("the log did not answer")
	if err := a.passes.Repair(t.Context()); err == nil {
		t.Fatal("a repair that could not pin the estate reported success")
	}
	st := a.passes.Status()
	if st.Repair.Completed || st.Repair.Error == "" || st.Repair.Report() != (RepairReport{}) {
		t.Fatalf("the unpinned pass = %+v, want incomplete with its error and no report", st.Repair)
	}
	if st.Repaired != complete {
		t.Fatalf("the last complete pass was replaced by an incomplete one: %+v", st.Repaired)
	}
}

// fetching is a peer whose fetches a test decides, chunk by chunk.
type fetching struct {
	fail map[objstore.Hash]error
}

func (p fetching) Fetch(_ context.Context, h objstore.Hash) ([]byte, error) {
	return nil, p.fail[h]
}

func (fetching) Has(context.Context, string, []objstore.Hash, bool) (transfer.Holding, error) {
	return transfer.Holding{}, transfer.ErrNoAnswer
}

// A CHUNK NO MEMBER COULD BE ASKED ABOUT IS NOT A LOST CHUNK. Missing counts
// only what every member answered it does not hold; one whose member did not
// answer is unreachable, still pending, and the pass answers an error so it is
// retried soon rather than at the next interval.
func TestRepairTellsMissingFromUnreachable(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	lost := objstore.HashOf([]byte("gone from every member"))
	away := objstore.HashOf([]byte("on a member that did not answer"))
	m := testMap(objplacement.MinPGBits, 1, "data-a")
	n, err := NewNode(NodeOptions{Self: "data-a", Local: store,
		Peers:      fetching{fail: map[objstore.Hash]error{lost: transfer.ErrNotFound, away: transfer.ErrUnreachable}},
		Layouts:    func() (*objplacement.Layout, bool) { return m.Layout(), true },
		References: References{newRefs(lost, away)}})
	if err != nil {
		t.Fatal(err)
	}
	err = n.Repair(t.Context())
	if err == nil || !strings.Contains(err.Error(), "could not be fetched") {
		t.Fatalf("Repair = %v, want an error naming what could not be fetched", err)
	}
	st := n.Status().Repair
	if !st.Completed || st.Missing != 1 || st.Unreachable != 1 || st.Pending != 2 || st.Placed != 2 {
		t.Fatalf("status = %+v, want 1 missing, 1 unreachable, both pending, the pass complete", st)
	}
}

// A REPAIR READS THE RUNS OF SLOTS THE MAP PLACES HERE, at every group count:
// each group this node holds exactly once, adjacent groups read together but
// never more than a 256th of the slots in one read.
func TestTheRunsARepairReadsAreExactlyThisNodesGroups(t *testing.T) {
	t.Parallel()
	for _, bits := range []int{objplacement.MinPGBits, 11, objplacement.MaxPGBits} {
		m := testMap(bits, 2, "data-a", "data-b", "data-c", "data-d", "data-e")
		l := m.Layout()
		runs := placedOn(l, "data-c")
		covered := map[int]bool{}
		for i, r := range runs {
			if r.Len() <= 0 || r.Len() > readSlots || r.Lo/readSlots != (r.Hi-1)/readSlots {
				t.Fatalf("%d bits: run %+v is not one bounded read", bits, r)
			}
			if i > 0 && runs[i-1].Hi > r.Lo {
				t.Fatalf("%d bits: runs out of order or overlapping at %+v", bits, r)
			}
			for slot := r.Lo; slot < r.Hi; slot++ {
				covered[l.Group(slot)] = true
			}
		}
		for pg := range m.Groups() {
			if held := slices.Contains(l.Up(pg), "data-c"); held != covered[pg] {
				t.Fatalf("%d bits: group %d held %v, read %v", bits, pg, held, covered[pg])
			}
		}
	}
}

// THE PASSES FOLLOW A MAP WHOSE GROUPS HAVE SPLIT: at more group bits a
// directory of the disk holds several groups with different holders, and each
// chunk is repaired to, and collected from, exactly the members its own group
// names.
func TestThePassesFollowASplitMap(t *testing.T) {
	t.Parallel()
	f := newFleetAt(t, 12, 1, "data-a", "data-b", "data-c")
	var written []objstore.Hash
	for i := range 60 {
		data := []byte{byte(i), 's', 'p'}
		h := objstore.HashOf(data)
		// Every chunk starts on every node: the repair must fetch none,
		// and the collection must leave exactly the placed copies.
		for _, d := range f.nodes {
			if err := d.disk.Put(h, data); err != nil {
				t.Fatal(err)
			}
		}
		written = append(written, h)
	}
	f.refs.refer(written...)
	for _, d := range f.nodes {
		if err := d.passes.Collect(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range written {
		want := f.upOf(h)
		for name, d := range f.nodes {
			if d.disk.Has(h) != slices.Contains(want, name) {
				t.Fatalf("chunk %s on %s: held %v, placed on %v", h, name, d.disk.Has(h), want)
			}
		}
	}
	for _, d := range f.nodes {
		if err := d.passes.Repair(t.Context()); err != nil {
			t.Fatal(err)
		}
		if st := d.passes.Status().Repair; st.Pending != 0 || st.Fetched != 0 || st.Placed == 0 {
			t.Fatalf("%s after collection: %+v, want everything placed held", d.name, st)
		}
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
	a.setNow(time.Now().Add(PendingGrace + time.Minute))
	if err := a.passes.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a.disk.Has(h) {
		t.Fatal("an unreferenced chunk past its grace survived")
	}
	if st := a.passes.Status().Collect; st.Deleted != 1 || st.Epoch != 3 || st.At.IsZero() {
		t.Fatalf("status = %+v, want 1 deleted at epoch 3", st)
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
	a.setNow(time.Now().Add(365 * 24 * time.Hour))
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
	a.setNow(time.Now().Add(PendingGrace + time.Minute))

	f.refs.barrierErr = errors.New("the log did not answer")
	if err := a.passes.Collect(t.Context()); err == nil {
		t.Fatal("a pass whose barrier failed reported success")
	}
	if !a.disk.Has(h) {
		t.Fatal("a pass that could not bring its view current collected a chunk")
	}
	if st := a.passes.Status().Collect; st.Error == "" || st.Skipped == "" {
		t.Fatalf("status = %+v, want the error and why nothing was collected", st)
	}

	f.refs.barrierErr = nil
	f.refs.incomplete = true
	if err := a.passes.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !a.disk.Has(h) {
		t.Fatal("a pass over an incomplete view collected a chunk")
	}
	if st := a.passes.Status().Collect; st.Skipped == "" {
		t.Fatalf("status = %+v, want the reason nothing was collected", st)
	}
}

// A NODE WITH NO MAP DELETES NOTHING: it cannot say what it should hold, and
// "nothing" is the one answer that would empty it.
func TestANodeWithNoMapDeletesNothing(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 1, "data-a")
	a := f.nodes["data-a"]
	a.layout.Store(nil)
	data := []byte("kept")
	h := objstore.HashOf(data)
	if err := a.disk.Put(h, data); err != nil {
		t.Fatal(err)
	}
	a.setNow(time.Now().Add(PendingGrace * 10))
	if err := a.passes.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !a.disk.Has(h) {
		t.Fatal("a node with no map collected a chunk")
	}
}

// A COPY BEYOND THIS NODE'S PLACEMENT IS DELETED WHEN EVERY MEMBER PLACING IT
// HOLDS AN INTACT COPY AND SAYS SO AT THE SAME EPOCH — and kept, and counted a
// stray, on any other answer.
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
	collect := func(why string, dropped bool) {
		t.Helper()
		if err := c.passes.Collect(t.Context()); err != nil {
			t.Fatal(err)
		}
		st := c.passes.Status().Collect
		if c.disk.Has(h) == dropped || st.Dropped != btoi(dropped) || st.Strays != btoi(!dropped) {
			t.Fatalf("%s: held %v, status %+v; want dropped %v", why, c.disk.Has(h), st, dropped)
		}
	}

	collect("one placing member does not hold it yet", false)

	// ONE PLACING MEMBER'S COPY HAS ROTTED: it may not vouch for ours.
	b := f.nodes["data-b"]
	if err := b.disk.Put(h, data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.disk.Root(), disk.Layout(h)), []byte("rot"), 0o600); err != nil {
		t.Fatal(err)
	}
	collect("a placing member's copy is rotten", false)
	if err := b.disk.Put(h, data); err != nil {
		t.Fatal(err)
	}

	// BOTH HOLD IT, BUT ONE PLACES BY ANOTHER EPOCH: kept.
	older := f.m
	older.Epoch--
	b.place(older)
	collect("a placing member places by another map", false)

	// ALL CONFIRM AT ONE EPOCH: dropped.
	b.place(f.m)
	collect("every placing member confirmed", true)
	for _, n := range []string{"data-a", "data-b"} {
		if !f.nodes[n].disk.Has(h) {
			t.Fatalf("%s lost its placed copy", n)
		}
	}
}

// A COLLECTION THAT STOPPED SHORT IS NOT THE LAST COMPLETE ONE: its counts
// cover only the slots it reached, and a stray count of zero from a pass that
// never looked would tell an operator a member taken out is empty. So the last
// pass that walked every slot is kept beside the last pass, and an incomplete
// one never replaces it.
func TestAnIncompleteCollectionIsNotTheLastCompleteOne(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2, "data-a", "data-b", "data-c")
	data, h := f.chunkPlaced(t, "data-a", "data-b")
	f.refs.refer(h)
	c := f.nodes["data-c"]
	if err := c.disk.Put(h, data); err != nil {
		t.Fatal(err)
	}
	if err := c.passes.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	complete := c.passes.Status().Collected
	if !complete.Completed || complete.Strays != 1 || complete.Epoch != f.m.Epoch ||
		c.passes.Status().Collect != complete {
		t.Fatalf("a complete pass keeping one stray = %+v", complete)
	}

	f.refs.barrierErr = errors.New("the log did not answer")
	if err := c.passes.Collect(t.Context()); err == nil {
		t.Fatal("a pass whose barrier failed reported success")
	}
	st := c.passes.Status()
	if st.Collect.Completed || st.Collect.Strays != 0 || st.Collect.Error == "" {
		t.Fatalf("setup: the failed pass = %+v, want incomplete and counting nothing", st.Collect)
	}
	if st.Collected != complete {
		t.Fatalf("the last complete pass was replaced by an incomplete one: %+v", st.Collected)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// answering is a peer whose answer about every chunk a test fixes.
type answering struct {
	held, placed bool
	epoch        uint64
	err          error

	mu       sync.Mutex
	asked    map[string]int
	unproven bool // a has that did not ask for a verified answer
}

func (a *answering) Fetch(context.Context, objstore.Hash) ([]byte, error) {
	return nil, transfer.ErrNotFound
}

func (a *answering) Has(_ context.Context, node string, hs []objstore.Hash, verify bool) (transfer.Holding, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.asked == nil {
		a.asked = map[string]int{}
	}
	a.asked[node]++
	a.unproven = a.unproven || !verify
	out := transfer.Holding{Node: node, Epoch: a.epoch}
	for range hs {
		out.Held = append(out.Held, a.held)
		out.Placed = append(out.Placed, a.placed)
	}
	return out, a.err
}

// strayNode is data-z holding one referenced chunk the map places elsewhere.
func strayNode(t *testing.T, m objplacement.Map, peer Peers) (*Node, *disk.Store, objstore.Hash) {
	t.Helper()
	store := openStore(t)
	data := []byte("a copy beyond the placement")
	h := objstore.HashOf(data)
	if err := store.Put(h, data); err != nil {
		t.Fatal(err)
	}
	l := m.Layout()
	n, err := NewNode(NodeOptions{Self: "data-z", Local: store, Peers: peer,
		Layouts:    func() (*objplacement.Layout, bool) { return l, true },
		References: References{newRefs(h)}})
	if err != nil {
		t.Fatal(err)
	}
	return n, store, h
}

// withMember is m with node added as a member that is out, so it holds no
// group and its every referenced copy is beyond its placement.
func withMember(m objplacement.Map, node string) objplacement.Map {
	m.Members = append(slices.Clone(m.Members), placement.Member{Node: node, Weight: 1,
		Share: placement.DefaultShare(1), Out: true})
	slices.SortFunc(m.Members, func(a, b placement.Member) int { return strings.Compare(a.Node, b.Node) })
	return m
}

// THE CONFIRMATION RULE, answer by answer. Only "I hold an intact copy, my own
// map places it here, and my map is yours" lets a copy go: a member that
// merely HOLDS a chunk may be holding a copy beyond its own placement, and
// dropping ours on its word is how two nodes reading different maps would
// each drop theirs. And every question asks for a VERIFIED answer.
func TestOnlyAFullConfirmationDropsACopy(t *testing.T) {
	t.Parallel()
	m := withMember(testMap(objplacement.MinPGBits, 1, "data-a"), "data-z")
	for _, c := range []struct {
		name    string
		peer    *answering
		dropped bool
	}{
		{"held, placed, same epoch", &answering{held: true, placed: true, epoch: 3}, true},
		{"held but not placed there", &answering{held: true, placed: false, epoch: 3}, false},
		{"placed but not held", &answering{held: false, placed: true, epoch: 3}, false},
		{"another epoch", &answering{held: true, placed: true, epoch: 4}, false},
		{"no answer", &answering{err: transfer.ErrNoAnswer}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, store, h := strayNode(t, m, c.peer)
			if err := n.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			if dropped := !store.Has(h); dropped != c.dropped {
				t.Fatalf("dropped = %v, want %v", dropped, c.dropped)
			}
			if c.peer.unproven {
				t.Fatal("a copy was confirmed by a member that was not asked to verify its own")
			}
		})
	}
}

// A NODE THE MAP DOES NOT HOLD DROPS NO REFERENCED COPY, however fully the
// members placing it confirm theirs: it has just come back and not been added
// again, and what it holds may be the copy they have not repaired yet. It
// still deletes what nothing references.
func TestANodeTheMapDoesNotHoldKeepsWhatIsReferenced(t *testing.T) {
	t.Parallel()
	peer := &answering{held: true, placed: true, epoch: 3}
	n, store, h := strayNode(t, testMap(objplacement.MinPGBits, 1, "data-a"), peer)
	junk := []byte("uploaded, never recorded")
	if err := store.Put(objstore.HashOf(junk), junk); err != nil {
		t.Fatal(err)
	}
	n.opts.Now = func() time.Time { return time.Now().Add(PendingGrace + time.Minute) }
	if err := n.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !store.Has(h) {
		t.Fatal("a node outside the map dropped a referenced copy")
	}
	if store.Has(objstore.HashOf(junk)) {
		t.Fatal("a node outside the map kept an unreferenced chunk past its grace")
	}
	st := n.Status().Collect
	if st.Strays != 1 || st.Dropped != 0 || st.Deleted != 1 || !strings.Contains(st.Skipped, "not a member") {
		t.Fatalf("status = %+v, want the stray kept and counted, and why", st)
	}
}

// A MEMBER ON PROBATION DROPS NO REFERENCED COPY either: the tick that trusts
// it gives it back most of the groups it held, so dropping them while it
// proves itself would copy its share away to copy it back a grace later. One
// an operator has also taken out is never placed on again whatever its
// probation says, and sheds its copies as any out member does.
func TestAMemberOnProbationKeepsWhatIsReferenced(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name           string
		out, probation bool
		kept           bool
	}{
		{"out: the control", true, false, false},
		{"on probation", false, true, true},
		{"on probation and out", true, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := withMember(testMap(objplacement.MinPGBits, 1, "data-a"), "data-z")
			for i := range m.Members {
				if m.Members[i].Node == "data-z" {
					m.Members[i].Out, m.Members[i].Probation = c.out, c.probation
				}
			}
			if keep, _ := KeepsEveryCopy(m, "data-z"); keep != c.kept {
				t.Fatalf("KeepsEveryCopy = %v, want %v", keep, c.kept)
			}
			n, store, h := strayNode(t, m, &answering{held: true, placed: true, epoch: 3})
			if err := n.Collect(t.Context()); err != nil {
				t.Fatal(err)
			}
			if store.Has(h) != c.kept {
				t.Fatalf("the copy kept = %v, want %v", store.Has(h), c.kept)
			}
			st := n.Status().Collect
			if c.kept && (st.Strays != 1 || !strings.Contains(st.Skipped, "probation")) {
				t.Fatalf("status = %+v, want the stray kept and counted, and why", st)
			}
		})
	}
	if keep, why := KeepsEveryCopy(testMap(objplacement.MinPGBits, 1, "data-a"), "data-z"); !keep ||
		!strings.Contains(why, "not a member") {
		t.Fatalf("a node the map does not hold: keeps %v, %q", keep, why)
	}
}

// A REMOVED HOLDER IS READ FROM THE TICK IT IS BACK. Two members holding every
// copy of a chunk go down together for longer than the grace and are removed;
// the chunk's group is re-placed on members that never had it. When the two
// return healthy, the map makes them members on probation on its first tick:
// placed on nothing, but ranked — so a reader finds the chunk, and the
// members it is now placed on repair it from them, with nothing counted
// missing. Kept out of the map while they proved themselves, they held the
// only copies of a chunk every reader and every repair read as lost, for a
// whole span after they were back.
func TestARemovedHolderIsReadFromTheTickItIsBack(t *testing.T) {
	t.Parallel()
	names := []string{"data-a", "data-b", "data-c", "data-d", "data-e"}
	f := newFleet(t, 2, names...)
	c := company(1, 2, "")
	var live []Presence
	for _, name := range names {
		live = append(live, up(name, 1))
	}
	state := first(t, live, c)
	var data []byte
	var h objstore.Hash
	l := state.Map.Layout()
	for i := 0; h == ""; i++ {
		b := []byte{byte(i), byte(i >> 8), 'r'}
		set := l.Up(l.Group(objstore.HashOf(b).Slot()))
		if slices.Equal(slices.Sorted(slices.Values(set)), []string{"data-a", "data-b"}) {
			data, h = b, objstore.HashOf(b)
		}
	}
	for _, holder := range []string{"data-a", "data-b"} {
		if err := f.nodes[holder].disk.Put(h, data); err != nil {
			t.Fatal(err)
		}
	}
	f.refs.refer(h)

	removed := ticks(t, state, without(live, "data-a", "data-b"), c, membership.OutTicks)
	if removed.Map.Holds("data-a") || removed.Map.Holds("data-b") {
		t.Fatalf("setup: the two are still members of %v", nodes(removed.Map))
	}
	back := tick(t, removed, live, c)
	for _, d := range f.nodes {
		d.place(back.Map)
	}
	bl := back.Map.Layout()
	now := bl.Up(bl.Group(h.Slot()))
	if slices.Contains(now, "data-a") || slices.Contains(now, "data-b") {
		t.Fatalf("setup: the chunk is placed on %v, a member on probation among them", now)
	}

	if got, err := f.nodes["data-e"].client.Fetch(t.Context(), h); err != nil ||
		string(got) != string(data) {
		t.Fatalf("a reader the tick they are back = %q, %v; want the chunk", got, err)
	}
	for _, holder := range now {
		p := f.nodes[holder].passes
		if err := p.Repair(t.Context()); err != nil {
			t.Fatal(err)
		}
		if st := p.Status().Repair; st.Missing != 0 || st.Fetched != 1 || !f.nodes[holder].disk.Has(h) {
			t.Fatalf("%s repaired %+v; want the chunk fetched from a member on probation", holder, st)
		}
	}
	// And what it held stays held while it proves itself.
	a := f.nodes["data-a"]
	if err := a.passes.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !a.disk.Has(h) {
		t.Fatal("the member on probation dropped a copy the map will give back to it")
	}
}

// A GROUP PLACED ON NOBODY — every member out — has no member to confirm a
// copy, and "every member confirmed" is not vacuously true of it.
func TestAGroupPlacedOnNobodyKeepsItsCopies(t *testing.T) {
	t.Parallel()
	m := testMap(objplacement.MinPGBits, 1, "data-a", "data-z")
	for i := range m.Members {
		m.Members[i].Out = true
	}
	n, store, h := strayNode(t, m, &answering{held: true, placed: true, epoch: 3})
	if err := n.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !store.Has(h) {
		t.Fatal("a copy was dropped with no member placed to hold it")
	}
}

// A MEMBER THAT DID NOT ANSWER IS ASKED ONCE A PASS, not once per group: every
// group it holds keeps its strays until the next pass, rather than each
// waiting out the same silence.
func TestASilentMemberIsAskedOncePerPass(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	m := withMember(testMap(objplacement.MinPGBits, 1, "data-a"), "data-z")
	r := newRefs()
	for i := range 40 {
		data := []byte{byte(i), 'q'}
		if err := store.Put(objstore.HashOf(data), data); err != nil {
			t.Fatal(err)
		}
		r.refer(objstore.HashOf(data))
	}
	peer := &answering{err: transfer.ErrNoAnswer}
	l := m.Layout()
	n, err := NewNode(NodeOptions{Self: "data-z", Local: store, Peers: peer,
		Layouts: func() (*objplacement.Layout, bool) { return l, true }, References: References{r}})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := peer.asked["data-a"]; got != 1 {
		t.Fatalf("the silent member was asked %d times in one pass, want once", got)
	}
	if st := n.Status().Collect; st.Strays != 40 || st.Dropped != 0 {
		t.Fatalf("status = %+v, want all 40 kept as strays", st)
	}
}

// A NODE WITH NO SOURCE OF REFERENCES IS REFUSED: it would read every chunk as
// unreferenced and delete the lot a day later.
func TestPassesWithNoSourceAreRefused(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	_, err := NewNode(NodeOptions{Self: "data-a", Local: store, Peers: &transfer.Client{},
		Layouts: func() (*objplacement.Layout, bool) { return nil, false }})
	if err == nil {
		t.Fatal("passes with no references were built")
	}
}
