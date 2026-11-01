package collect

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeSource is one domain's references, held in memory — all of them in a
// required table, so its walk hands on every one (standing_test.go has what a
// retired or derived table does, through the real declarations).
type fakeSource struct {
	mu    sync.Mutex
	named map[objstore.Key]objstore.Reference
	// incomplete is whether every read reports itself incomplete, and
	// incompleteAfter how many judged batches answer complete first.
	incomplete      bool
	incompleteAfter int
	judged          int
	barriers        int
	// pageRead, when set, is called as each page of a walk is read, with
	// the source's lock held.
	pageRead func()
	// barrierErr is what every barrier answers, when set.
	barrierErr error
}

func (s *fakeSource) Name() string { return "tracker" }

func (s *fakeSource) Barrier(context.Context) (statelog.Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.barriers++
	return statelog.Position{}, s.barrierErr
}

func (s *fakeSource) Referenced(_ context.Context, among []objstore.Key,
	_ statelog.Position) (map[objstore.Key]struct{}, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[objstore.Key]struct{}{}
	for _, k := range among {
		if _, ok := s.named[k]; ok {
			out[k] = struct{}{}
		}
	}
	s.judged++
	if s.incompleteAfter > 0 && s.judged > s.incompleteAfter {
		return out, false, nil
	}
	return out, !s.incomplete, nil
}

// Required is one walk over every reference the source names, in key order, a
// page of [objstore.WalkPage] at a time — read when the walk is asked, as an estate's
// is, so a reference named after the walk began and before its page is read is
// in it.
func (s *fakeSource) Required(statelog.Position) []objstore.Walk {
	return []objstore.Walk{&fakeWalk{s: s}}
}

// Moved is the same walk: every reference the source names is a required
// table's, and a store move walks the required tables too.
func (s *fakeSource) Moved(at statelog.Position) []objstore.Walk { return s.Required(at) }

// fakeWalk is a [fakeSource]'s walk, keyset-paged on the key as a table's is.
type fakeWalk struct {
	s     *fakeSource
	after objstore.Key
	begun bool
	done  bool
}

func (w *fakeWalk) Next(context.Context) ([]objstore.Reference, bool, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	if w.done {
		return nil, !w.s.incomplete, nil
	}
	if w.s.pageRead != nil {
		w.s.pageRead()
	}
	var page []objstore.Reference
	for _, ref := range slices.SortedFunc(maps.Values(w.s.named), func(a, b objstore.Reference) int {
		return a.Object.Key.Compare(b.Object.Key)
	}) {
		if w.begun && ref.Object.Key.Compare(w.after) <= 0 {
			continue
		}
		page = append(page, ref)
		if len(page) == objstore.WalkPage {
			break
		}
	}
	if len(page) < objstore.WalkPage {
		w.done = true
	}
	if len(page) > 0 {
		w.after, w.begun = page[len(page)-1].Object.Key, true
	}
	return page, !w.s.incomplete, nil
}

// name records a row naming o, owned by the file at path.
func (s *fakeSource) name(o objstore.Object, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.named[o.Key] = objstore.Reference{Object: o, NamedBy: "ENG/" + path}
}

// unname drops the row naming k, as a file's removal or rewrite does.
func (s *fakeSource) unname(k objstore.Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.named, k)
}

// clock is a settable instant every piece of a case reads.
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
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// backend is the twin with what a real backend has and the twin does not:
// uploads begun and never finished, a store that answers "not found" for an
// object it has not caught up with yet, and one whose questions fail.
//
// IT KEEPS NO ORDER — the twin's ranged listing is not promoted through the
// interface it is held by — so the audit reads its whole listing a window at a
// time, as it reads the broker's object store, and asks about an object by
// name only for a second look or once a listing has failed; a case auditing a
// store that lists ranges wraps it in a [ranged].
type backend struct {
	objstore.Backend
	mem *memobj.Backend

	mu         sync.Mutex
	pending    []objstore.Pending
	pendingErr error
	// listTail is what a listing answers once it has handed on every
	// object — an S3 gateway's ErrUndated, say — and listErr what it
	// answers before handing on any.
	listTail, listErr error
	// lists is how many listings the store was asked for. onList, when set,
	// is called as a listing ends — once it has handed on every name and
	// before it answers, outside every lock, so it may block — with which
	// listing this is, counting from one, and an error it returns is the
	// listing's.
	lists  int
	onList func(ctx context.Context, nth int) error
	// hidden is the names a listing leaves out, as one that has not caught
	// up with an upload, or a gateway that could not date it, does; and
	// nothing is the names it hands on holding nothing — no bytes and no
	// digest, as the broker's listing hands on a name whose metadata does
	// not decode or that only a delete marker holds — each with what a stat
	// of it answers.
	hidden  map[string]bool
	nothing map[string]error
	// lagging is how many more times a stat of a name answers "not found"
	// before it answers what the twin holds.
	lagging map[string]int
	statErr error
	// asks is how many times each name has been asked about. onStat, when
	// set, is called before a stat is answered — outside every lock, so it
	// may block — with the name and which ask of it this is, counting from
	// one, and an error it returns is the answer; onDelete is the same for a
	// deletion.
	asks     map[string]int
	onStat   func(ctx context.Context, name string, nth int) error
	onDelete func(ctx context.Context, name string) error
}

func (b *backend) Pending(ctx context.Context, visit func(objstore.Pending) error) error {
	b.mu.Lock()
	held, failure := slices.Clone(b.pending), b.pendingErr
	b.mu.Unlock()
	if failure != nil {
		return failure
	}
	for _, p := range held {
		if err := visit(p); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (b *backend) List(ctx context.Context, visit func(objstore.Info) error) error {
	b.mu.Lock()
	b.lists++
	nth, hook, failure := b.lists, b.onList, b.listErr
	b.mu.Unlock()
	if failure != nil {
		return failure
	}
	if err := b.Backend.List(ctx, func(info objstore.Info) error {
		b.mu.Lock()
		hide := b.hidden[info.Name]
		_, empty := b.nothing[info.Name]
		b.mu.Unlock()
		switch {
		case hide:
			return nil
		case empty:
			return visit(objstore.Info{Name: info.Name, Written: info.Written})
		}
		return visit(info)
	}); err != nil {
		return err
	}
	if hook != nil {
		if err := hook(ctx, nth); err != nil {
			return err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.listTail
}

// hide leaves names out of every listing from now on.
func (b *backend) hide(names ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, name := range names {
		b.hidden[name] = true
	}
}

// holdNothing has listings hand name on holding nothing, and a stat of it
// answer answer.
func (b *backend) holdNothing(name string, answer error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nothing[name] = answer
}

// listings is how many listings the store has been asked for.
func (b *backend) listings() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lists
}

func (b *backend) Abandon(_ context.Context, p objstore.Pending) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = slices.DeleteFunc(b.pending, func(q objstore.Pending) bool { return q.ID == p.ID })
	return nil
}

func (b *backend) Stat(ctx context.Context, name string) (objstore.Info, error) {
	b.mu.Lock()
	b.asks[name]++
	nth, hook := b.asks[name], b.onStat
	b.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, name, nth); err != nil {
			return objstore.Info{}, err
		}
	}
	b.mu.Lock()
	failure := b.statErr
	lag := b.lagging[name]
	if lag > 0 {
		b.lagging[name] = lag - 1
	}
	answer, empty := b.nothing[name]
	b.mu.Unlock()
	switch {
	case failure != nil:
		return objstore.Info{}, failure
	case lag > 0:
		return objstore.Info{}, objstore.ErrNotFound
	case empty:
		return objstore.Info{}, answer
	}
	return b.Backend.Stat(ctx, name)
}

func (b *backend) Delete(ctx context.Context, name string) error {
	b.mu.Lock()
	hook := b.onDelete
	b.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, name); err != nil {
			return err
		}
	}
	return b.Backend.Delete(ctx, name)
}

// Corrupt replaces name's bytes, keeping its digest — [memobj.Backend.Corrupt].
func (b *backend) Corrupt(name string, data []byte) { b.mem.Corrupt(name, data) }

func (b *backend) begin(p objstore.Pending) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, p)
}

func (b *backend) pendingIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, p := range b.pending {
		out = append(out, p.ID)
	}
	slices.Sort(out)
	return out
}

type harness struct {
	t       *testing.T
	clock   *clock
	backend *backend
	store   *objstore.Store
	source  *fakeSource
	c       *Collector
}

// newHarness is a collector over an in-memory store whose writes are dated,
// and whose keys are minted, at one settable clock.
func newHarness(t *testing.T) *harness {
	t.Helper()
	clk := &clock{now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	mem := memobj.NewAt(clk.Now)
	b := &backend{Backend: mem, mem: mem, lagging: map[string]int{}, asks: map[string]int{},
		hidden: map[string]bool{}, nothing: map[string]error{}}
	store, err := objstore.NewStoreAt(b, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{named: map[objstore.Key]objstore.Reference{}}
	c, err := New(Options{Store: store, References: References{src}, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, clock: clk, backend: b, store: store, source: src, c: c}
}

func (h *harness) put(content string) objstore.Key { return h.putObject(content).Key }

func (h *harness) putObject(content string) objstore.Object {
	h.t.Helper()
	o, err := h.store.Put(h.t.Context(), strings.NewReader(content), 1<<20, objstore.PutMeta{})
	if err != nil {
		h.t.Fatal(err)
	}
	return o
}

// file stores content and names it by a row, as a file's upload and record do.
func (h *harness) file(path, content string) objstore.Object {
	h.t.Helper()
	o := h.putObject(content)
	h.source.name(o, path)
	return o
}

// putAt stores content under a key minted at minted, written at the clock's
// instant — an upload whose bytes landed long after its key was minted, or
// one whose key a clock running ahead minted.
func (h *harness) putAt(minted time.Time, content string) objstore.Key {
	h.t.Helper()
	k := objstore.KeyAt(minted)
	if err := h.backend.Put(h.t.Context(), k.Name(), strings.NewReader(content),
		objstore.PutMeta{}); err != nil {
		h.t.Fatal(err)
	}
	return k
}

// askByName fails every listing the store is asked for, so the audit takes
// its first look at each object BY NAME: a store that keeps no order is asked
// so from the window whose listing failed, and a case about what a stat's
// answer makes of an object takes its first look the way the stat is asked.
// The failed listing is one failed question, which the first answer after it
// ends ([failedInARow]).
func (h *harness) askByName() {
	h.backend.mu.Lock()
	defer h.backend.mu.Unlock()
	h.backend.listErr = errListingFailed
}

var errListingFailed = errors.New("the listing failed: the replica it was read from is behind")

func (h *harness) held(k objstore.Key) bool {
	h.t.Helper()
	_, err := h.backend.Backend.Stat(h.t.Context(), k.Name())
	if errors.Is(err, objstore.ErrNotFound) {
		return false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return true
}

// AN OBJECT NOTHING NAMES GOES ONCE IT IS PAST THE GRACE, and not before:
// every object is unnamed from its upload until its file's record lands.
func TestAnUnnamedObjectGoesOnlyAfterTheGrace(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	orphan := h.put("an upload whose record never came")
	named := h.file("a.md", "a file's bytes").Key

	h.clock.advance(PendingGrace - time.Minute)
	if r, err := h.c.Collect(t.Context()); err != nil || r.Deleted != 0 || !r.Completed {
		t.Fatalf("a pass inside the grace = %+v, %v", r, err)
	}
	if !h.held(orphan) {
		t.Fatal("an object inside its grace was deleted")
	}
	h.clock.advance(2 * time.Minute)
	r, err := h.c.Collect(t.Context())
	if err != nil || r.Deleted != 1 || r.Referenced != 1 || !r.Completed {
		t.Fatalf("a pass past the grace = %+v, %v", r, err)
	}
	if h.held(orphan) {
		t.Fatal("an unnamed object past its grace was kept")
	}
	if !h.held(named) {
		t.Fatal("an object a file names was deleted")
	}
}

// AN OBJECT UNNAMED ONCE IT IS A DAY OLD GOES AT THE NEXT PASS, with no
// further day's wait: the grace dates the object's UPLOAD, the window between
// storing it and writing the row that names it, and nothing records when a row
// stopped naming it. A file older than a day that is rewritten or removed
// loses its old bytes within the hour — which is what the object store's docs
// promise, and why the store is no place to recover a replaced version from.
func TestAnObjectUnnamedOnceItIsADayOldGoesAtTheNextPass(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := h.file("a.md", "the version a rewrite replaces").Key

	h.clock.advance(PendingGrace + time.Hour)
	if r, err := h.c.Collect(t.Context()); err != nil || r.Deleted != 0 || r.Referenced != 1 {
		t.Fatalf("a pass while a row names the object = %+v, %v", r, err)
	}
	h.source.unname(old)
	h.clock.advance(CollectInterval)
	r, err := h.c.Collect(t.Context())
	if err != nil || r.Deleted != 1 || !r.Completed {
		t.Fatalf("the first pass after the row stopped naming it = %+v, %v; "+
			"want the object deleted then, not a day after it was unnamed", r, err)
	}
	if h.held(old) {
		t.Fatal("an object past its grace was kept after nothing named it")
	}
}

// A LISTING THAT LEFT UNDATED NAMES OUT STILL HAS WHAT IT DATED JUDGED. A
// gateway that omits an object's date fails the listing with ErrUndated once
// it has handed on everything else, and the batch those filled is judged
// before the pass reports the failure — or every collection on such a bucket
// would end without deleting its last batch.
func TestAnUndatedListingStillJudgesWhatItDated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	orphan := h.put("an upload whose record never came")
	h.clock.advance(PendingGrace + time.Minute)
	h.backend.listTail = fmt.Errorf("%w: one object", objstore.ErrUndated)

	r, err := h.c.Collect(t.Context())
	if !errors.Is(err, objstore.ErrUndated) || r.Completed || r.Error == "" {
		t.Fatalf("a pass over an undated listing = %+v, %v; want it failed with ErrUndated", r, err)
	}
	if r.Deleted != 1 || h.held(orphan) {
		t.Fatalf("the pass deleted %d; want the dated orphan judged and deleted", r.Deleted)
	}
}

// AN OBJECT GOES ONLY ONCE IT IS PAST THE GRACE BY BOTH CLOCKS — the
// backend's, which dates its bytes, and its key's own, which is what the
// bound on the write naming it is measured from. Either alone leaves a case
// in which a write may still name the object after the collection judged it.
func TestAnObjectIsJudgedByBothItsInstants(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	start := h.clock.Now()
	// MINTED LONG BEFORE ITS BYTES LANDED: an upload that took most of a
	// day. Its key is past the grace well before its bytes are.
	slow := h.putAt(start.Add(-PendingGrace), "a slow upload")
	// MINTED AHEAD OF THE STORE'S CLOCK, by a node whose clock runs fast:
	// its bytes are past the grace before its key is.
	ahead := h.putAt(start.Add(6*time.Hour), "a key from a fast clock")

	h.clock.advance(PendingGrace + time.Hour)
	r, err := h.c.Collect(t.Context())
	if err != nil || !r.Completed {
		t.Fatalf("Collect = %+v, %v", r, err)
	}
	if !h.held(ahead) {
		t.Fatal("an object whose key is inside the grace was deleted on its bytes' age alone")
	}
	if h.held(slow) {
		t.Fatal("an unnamed object past the grace by both instants was kept")
	}
	h.clock.advance(6 * time.Hour)
	if _, err := h.c.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h.held(ahead) {
		t.Fatal("an unnamed object past the grace by both instants was kept")
	}

	// AND THE BYTES' AGE BINDS TOO: a key minted long ago whose bytes landed
	// a moment ago is kept until they are past the grace.
	late := h.putAt(h.clock.Now().Add(-2*PendingGrace), "bytes that just landed")
	h.clock.advance(time.Hour)
	if _, err := h.c.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !h.held(late) {
		t.Fatal("an object whose bytes are inside the grace was deleted on its key's age alone")
	}
}

// A NAME THAT IS NOT AN OBJECT'S KEY IS NEVER THE COLLECTOR'S: a backend lists
// every name it holds, and the collector — the one reader of the grammar —
// neither counts nor judges nor deletes an object it did not name, however
// old. That includes a bare digest, which is no key the engine mints, and a
// bare UUID outside the engine's namespace, which is what another application
// sharing an S3 bucket under an empty prefix names its own objects with.
func TestANameThatIsNotAKeyIsLeftAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := objstore.KeyAt(h.clock.Now())
	foreign := []string{
		"somebody-elses.txt",
		string(objstore.HashOf([]byte("a digest"))),
		old.String(),
		"files/" + strings.ToUpper(old.String()),
		"files/" + uuid.New().String(),
		"other/" + old.String(),
	}
	for _, name := range foreign {
		if err := h.backend.Put(t.Context(), name, strings.NewReader("theirs"),
			objstore.PutMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	orphan := h.put("an unnamed object")
	h.clock.advance(PendingGrace + time.Hour)
	r, err := h.c.Collect(t.Context())
	if err != nil || r.Listed != 1 || r.Deleted != 1 || !r.Completed {
		t.Fatalf("the pass = %+v, %v; want the one object listed and deleted", r, err)
	}
	if h.held(orphan) {
		t.Fatal("the unnamed object was kept")
	}
	for _, name := range foreign {
		if _, err := h.backend.Stat(t.Context(), name); err != nil {
			t.Errorf("%q, which is not an object's key, is gone: %v", name, err)
		}
	}
}

// AN ESTATE THAT IS NOT COMPLETE DELETES NOTHING: a record this node could not
// apply may be the one naming the object.
func TestAnIncompleteEstateDeletesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	orphan := h.put("orphan")
	h.source.incomplete = true
	h.clock.advance(PendingGrace + time.Hour)
	r, err := h.c.Collect(t.Context())
	if err != nil || r.Deleted != 0 || r.Skipped == "" || r.Completed {
		t.Fatalf("a pass over an incomplete estate = %+v, %v", r, err)
	}
	if !h.held(orphan) {
		t.Fatal("an object was deleted on the word of an incomplete estate")
	}
}

// TWO COLLECTORS AT ONCE ARE SAFE: a deletion takes no lock, and deleting
// what the other already deleted is not an error — a duty that moved mid-pass
// costs requests, never a failed pass or a kept object.
func TestTwoCollectorsAtOnceAreSafe(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var orphans []objstore.Key
	for i := range 40 {
		orphans = append(orphans, h.put("orphan "+time.Duration(i).String()))
	}
	h.clock.advance(PendingGrace + time.Hour)
	other, err := New(Options{Store: h.store, References: References{h.source}, Now: h.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, c := range []*Collector{h.c, other} {
		wg.Go(func() {
			if r, err := c.Collect(t.Context()); err != nil || !r.Completed {
				t.Errorf("a collector beside another = %+v, %v", r, err)
			}
		})
	}
	wg.Wait()
	for _, k := range orphans {
		if h.held(k) {
			t.Fatalf("%s survived two collectors", k)
		}
	}
}

// THE BARRIER IS TAKEN BEFORE THE FIRST JUDGEMENT, once a pass, so the estate
// every batch is judged by is the same one and at least as new as the moment
// the pass began.
//
// Mutation: pin per batch, or not at all, and the count is wrong.
func TestAPassPinsTheEstateBeforeItJudges(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for i := range JudgeBatch + 1 {
		h.put(fmt.Sprintf("orphan-%d", i))
	}
	h.clock.advance(PendingGrace + time.Hour)
	if _, err := h.c.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h.source.barriers != 1 {
		t.Fatalf("%d barriers over two batches, want one per pass", h.source.barriers)
	}
}

// AN ESTATE THAT CANNOT BE BROUGHT CURRENT DELETES NOTHING, and the pass says
// why: judged by a view older than the pass, an object a record named a moment
// ago would read as nobody's.
func TestAPassWhoseBarrierFailsDeletesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	orphan := h.put("orphan")
	h.source.barrierErr = errors.New("the log is at its ceiling")
	h.clock.advance(PendingGrace + time.Hour)
	r, err := h.c.Collect(t.Context())
	if err == nil || r.Deleted != 0 || r.Completed || !strings.Contains(r.Skipped, "ceiling") {
		t.Fatalf("a pass whose barrier failed = %+v, %v; want the failure, reported, "+
			"and nothing deleted", r, err)
	}
	if !h.held(orphan) {
		t.Fatal("an object was deleted by a pass that could not bring its estate current")
	}
}

// A PASS WITH NOTHING PAST THE GRACE TAKES NO BARRIER: it decides nothing, and
// a barrier is a record on the domain's log. Taken anyway, every pass wrote a
// record to every estate's log each hour, and a node's first pass wrote one to
// logs nothing else had ever written to, at boot.
//
// Mutation: pin before the listing again, and this fails.
func TestAPassWithNothingToJudgeWritesNoBarrier(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.put("young") // uploaded now: inside the grace, so never judged
	r, err := h.c.Collect(t.Context())
	if err != nil || !r.Completed || r.Aged != 0 {
		t.Fatalf("a pass over a young store = %+v, %v", r, err)
	}
	if h.source.barriers != 0 {
		t.Fatalf("a pass that judged nothing took %d barrier(s), want none", h.source.barriers)
	}
}

// MORE OBJECTS THAN ONE BATCH are all judged, the last partial batch
// included.
func TestEveryBatchIsJudged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var keep []objstore.Key
	for i := range JudgeBatch*2 + 7 {
		if i%3 == 0 {
			keep = append(keep, h.file(fmt.Sprintf("%d.md", i), "object "+time.Duration(i).String()).Key)
			continue
		}
		h.put("object " + time.Duration(i).String())
	}
	h.clock.advance(PendingGrace + time.Hour)
	r, err := h.c.Collect(t.Context())
	if err != nil || !r.Completed {
		t.Fatalf("Collect = %+v, %v", r, err)
	}
	if r.Listed != JudgeBatch*2+7 || r.Deleted+r.Referenced != r.Listed || r.Referenced != len(keep) {
		t.Fatalf("the pass judged %+v; want every object judged and %d kept", r, len(keep))
	}
	for _, k := range keep {
		if !h.held(k) {
			t.Fatalf("a named object past the first batch was deleted")
		}
	}
}

// A COLLECTOR WITH NOTHING TO READ REFERENCES FROM IS REFUSED: it would read
// every object as unreferenced and delete the company's files a day later.
func TestACollectorWithNoReferencesIsRefused(t *testing.T) {
	t.Parallel()
	store, err := objstore.NewStore(memobj.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Store: store}); err == nil {
		t.Fatal("a collector with no references was built")
	}
}

// AN UPLOAD THAT NEVER FINISHED IS ABANDONED ONCE IT BEGAN MORE THAN THE GRACE
// AGO — when nothing names it, or a key also minted past the grace does — and
// never when somebody else's name does. Every upload in flight is pending
// too, which is what the grace is for.
func TestAnUnfinishedUploadIsAbandonedOnlyAfterTheGrace(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	start := h.clock.Now()
	oldKey := objstore.KeyAt(start)
	freshKey := objstore.KeyAt(start.Add(PendingGrace))
	for _, p := range []objstore.Pending{
		{ID: "nameless", Started: start},
		{ID: "old-key", Name: oldKey.Name(), Started: start},
		// BEGUN LONG AGO BY THE BACKEND'S CLOCK, under a key a fast
		// clock minted: its key is inside the grace.
		{ID: "fresh-key", Name: freshKey.Name(), Started: start},
		{ID: "theirs", Name: "somebody-elses/upload.bin", Started: start},
	} {
		h.backend.begin(p)
	}
	h.clock.advance(PendingGrace - time.Minute)
	h.backend.begin(objstore.Pending{ID: "in-flight", Started: h.clock.Now()})

	if r, err := h.c.Collect(t.Context()); err != nil || r.Abandoned != 0 {
		t.Fatalf("a sweep inside the grace = %+v, %v; want nothing abandoned", r, err)
	}
	h.clock.advance(2 * time.Minute)
	r, err := h.c.Collect(t.Context())
	if err != nil || r.Abandoned != 2 {
		t.Fatalf("a sweep past the grace = %+v, %v; want the nameless upload and the old key's", r, err)
	}
	if got, want := h.backend.pendingIDs(), []string{"fresh-key", "in-flight", "theirs"}; !slices.Equal(got, want) {
		t.Fatalf("pending after the sweep = %q, want %q", got, want)
	}
	h.clock.advance(PendingGrace)
	if r, err = h.c.Collect(t.Context()); err != nil || r.Abandoned != 2 {
		t.Fatalf("a later sweep = %+v, %v; want the fresh key's and the one that was "+
			"in flight", r, err)
	}
	if got := h.backend.pendingIDs(); !slices.Equal(got, []string{"theirs"}) {
		t.Fatalf("pending = %q; somebody else's upload is never abandoned", got)
	}
}

// A SWEEP THE BACKEND REFUSES IS REPORTED, AND THE COLLECTION STANDS: an S3
// identity without the right to list or abort uploads is a sweep that cannot
// run, never a collection that did not.
func TestARefusedSweepIsReportedBesideTheCollection(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	orphan := h.put("an orphan")
	h.backend.pendingErr = errors.New("AccessDenied: s3:ListBucketMultipartUploads")
	h.clock.advance(PendingGrace + time.Hour)
	r, err := h.c.Collect(t.Context())
	if err != nil || !r.Completed || r.Deleted != 1 || !strings.Contains(r.SweepError, "AccessDenied") {
		t.Fatalf("a collection whose sweep was refused = %+v, %v; want it complete and the "+
			"refusal reported", r, err)
	}
	if h.held(orphan) {
		t.Fatal("the collection did not run beside a refused sweep")
	}
}

// A PASS THAT DELETED AND THEN MET AN INCOMPLETE ESTATE SAYS BOTH: what it
// deleted before it stopped is gone, and a report that read "deleted nothing"
// would have the operator believe otherwise.
func TestAPassThatStoppedReportsWhatItDeletedFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for i := range JudgeBatch + 10 {
		h.put(fmt.Sprintf("orphan %d", i))
	}
	h.source.incompleteAfter = 1
	h.clock.advance(PendingGrace + time.Hour)
	r, err := h.c.Collect(t.Context())
	if err != nil || r.Deleted != JudgeBatch || r.Skipped == "" || r.Completed {
		t.Fatalf("the pass = %+v, %v; want one batch deleted and why it stopped", r, err)
	}
	if got := h.c.Status().Collect; got.Deleted != JudgeBatch || got.Skipped == "" {
		t.Fatalf("the status = %+v; want both halves", got)
	}
}

// THE AUDIT FINDS WHAT THE STORE LOST AND WHAT IT HOLDS WRONG, and names each
// as the file it belongs to: an object it does not hold, one it holds at
// another size, and one it holds under another digest.
func TestTheAuditFindsMissingAndDamagedFiles(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.file("kept.md", "kept")
	lost := h.file("lost.md", "lost")
	short := h.file("short.md", "the whole of it")
	bent := h.putObject("one digest")
	bent.Hash = objstore.HashOf([]byte("another digest"))
	h.source.name(bent, "bent.md")
	if err := h.store.Delete(t.Context(), lost.Key); err != nil {
		t.Fatal(err)
	}
	h.backend.Corrupt(short.Key.Name(), []byte("part"))

	r, err := h.c.Audit(t.Context())
	if err != nil || !r.Completed || r.Referenced != 4 || r.Missing != 1 || r.Damaged != 2 {
		t.Fatalf("Audit = %+v, %v; want one missing and two damaged of four", r, err)
	}
	want := []MissingFile{
		{Object: bent.Key, NamedBy: "ENG/bent.md", Finding: FindingDamaged},
		{Object: lost.Key, NamedBy: "ENG/lost.md", Finding: FindingMissing},
		{Object: short.Key, NamedBy: "ENG/short.md", Finding: FindingDamaged},
	}
	if r.Found == nil || r.Found.Missing != 1 || r.Found.Damaged != 2 ||
		!slices.Equal(r.Found.MissingFiles, want) {
		t.Fatalf("the findings = %+v, want %+v", r.Found, want)
	}
}

// A NAME THE STORE HOLDS NO READABLE OBJECT UNDER IS DAMAGED, ON ONE ANSWER:
// the store said so definitely ([objstore.ErrCorrupt]) — metadata that does
// not decode, on the broker — and asking again reads the same. Taken for a
// failure, it stopped every audit at that object and never named the file,
// and each retry asked the whole store again up to it.
func TestANameHoldingNoReadableObjectIsDamaged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.askByName()
	h.file("kept.md", "kept")
	spoiled := h.file("spoiled.md", "bytes the store can no longer read")
	h.backend.onStat = func(_ context.Context, name string, _ int) error {
		if name == spoiled.Key.Name() {
			return fmt.Errorf("%w: its metadata does not decode", objstore.ErrCorrupt)
		}
		return nil
	}
	r, err := h.c.Audit(t.Context())
	if err != nil || !r.Completed || r.Referenced != 2 || r.Damaged != 1 || r.Missing != 0 {
		t.Fatalf("Audit = %+v, %v; want it complete with the one file damaged", r, err)
	}
	want := []MissingFile{{Object: spoiled.Key, NamedBy: "ENG/spoiled.md", Finding: FindingDamaged}}
	if r.Found == nil || r.Found.Damaged != 1 || !slices.Equal(r.Found.MissingFiles, want) {
		t.Fatalf("the findings = %+v, want %+v", r.Found, want)
	}
	h.backend.mu.Lock()
	defer h.backend.mu.Unlock()
	if n := h.backend.asks[spoiled.Key.Name()]; n != 1 {
		t.Fatalf("the spoiled name was asked about %d times; a definite answer needs no second look", n)
	}
}

// A NAME THE LISTING HANDS ON HOLDING NOTHING IS ASKED ABOUT BY NAME, and the
// stat's answer is the finding: the broker's listing hands on a name whose
// metadata does not decode, and one only a delete marker holds, as an object
// of no bytes and no digest. Judged from the listing, a marker over a file's
// object read as DAMAGED where the object is gone, and a spoiled empty file's
// as HELD, since an empty object of no digest is what the listing said.
func TestANameListedHoldingNothingIsAskedAboutByName(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	kept := h.file("kept.md", "kept")
	marked := h.file("marked.md", "an object a delete marker replaced")
	h.backend.holdNothing(marked.Key.Name(), objstore.ErrNotFound)
	spoiledEmpty := h.file("empty.md", "")
	h.backend.holdNothing(spoiledEmpty.Key.Name(), fmt.Errorf("%w: its metadata does not decode", objstore.ErrCorrupt))
	spoiled := h.file("spoiled.md", "bytes the store can no longer read")
	h.backend.holdNothing(spoiled.Key.Name(), fmt.Errorf("%w: its metadata does not decode", objstore.ErrCorrupt))

	r, err := h.c.Audit(t.Context())
	if err != nil || !r.Completed || r.Referenced != 4 || r.Missing != 1 || r.Damaged != 2 {
		t.Fatalf("Audit = %+v, %v; want the marked file missing and both spoiled ones damaged", r, err)
	}
	want := []MissingFile{
		{Object: spoiledEmpty.Key, NamedBy: "ENG/empty.md", Finding: FindingDamaged},
		{Object: marked.Key, NamedBy: "ENG/marked.md", Finding: FindingMissing},
		{Object: spoiled.Key, NamedBy: "ENG/spoiled.md", Finding: FindingDamaged},
	}
	if r.Found == nil || !slices.Equal(r.Found.MissingFiles, want) {
		t.Fatalf("the findings = %+v, want %+v", r.Found, want)
	}
	h.backend.mu.Lock()
	defer h.backend.mu.Unlock()
	for _, o := range []objstore.Object{marked, spoiledEmpty, spoiled} {
		if n := h.backend.asks[o.Key.Name()]; n != 1 {
			t.Errorf("%s, listed holding nothing, was asked about %d times, want once", o.Key, n)
		}
	}
	if n := h.backend.asks[kept.Key.Name()]; n != 0 || h.backend.lists != 1 {
		t.Errorf("the audit asked about the listed file %d times over %d listings, want none "+
			"over one", n, h.backend.lists)
	}
}

// A STORE THAT HAD NOT CAUGHT UP IS NOT ONE THAT LOST THE OBJECT: a "not found"
// to a question by name is asked again at the end of the pass before the
// object is called missing.
func TestALaggingNotFoundIsNotMissing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.askByName()
	o := h.file("new.md", "just uploaded")
	h.backend.lagging[o.Key.Name()] = 1
	r, err := h.c.Audit(t.Context())
	if err != nil || r.Missing != 0 || r.Found == nil || r.Found.Missing != 0 {
		t.Fatalf("Audit = %+v, %v; an object answered once as not found and then found "+
			"is not missing", r, err)
	}
	h.backend.lagging[o.Key.Name()] = 2
	if r, err = h.c.Audit(t.Context()); err != nil || r.Missing != 1 {
		t.Fatalf("Audit = %+v, %v; an object not found twice is missing", r, err)
	}
}

// AND AN OBJECT NO LISTING SHOWED IS ASKED ABOUT BY NAME before it is missing:
// a listing that left it out — a store that had not caught up with it, a
// gateway that could not date it — is not a store that lost it, and one the
// store then answers is judged as it holds it.
func TestAnUnlistedObjectIsAskedAboutBeforeItIsMissing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.file("new.md", "just uploaded")
	h.backend.hide(o.Key.Name())
	h.backend.listTail = objstore.ErrUndated
	r, err := h.c.Audit(t.Context())
	if err != nil || r.Missing != 0 || r.Found == nil || r.Found.Missing != 0 {
		t.Fatalf("Audit = %+v, %v; an object a listing left out and a stat found "+
			"is not missing", r, err)
	}
	h.backend.lagging[o.Key.Name()] = 1
	if r, err = h.c.Audit(t.Context()); err != nil || r.Missing != 1 {
		t.Fatalf("Audit = %+v, %v; an object neither listed nor found is missing", r, err)
	}
}

// THE AUDIT READS THE LISTING, NOT A QUESTION PER OBJECT: a store that keeps no
// order holding every file it should is listed once and asked nothing else.
func TestTheAuditReadsAListingRatherThanAStatPerObject(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for i := range 20 {
		h.file(fmt.Sprintf("f%02d.md", i), fmt.Sprintf("content %d", i))
	}
	r, err := h.c.Audit(t.Context())
	if err != nil || !r.Completed || r.Referenced != 20 || r.Missing != 0 || r.Damaged != 0 {
		t.Fatalf("Audit = %+v, %v", r, err)
	}
	if stats, lists := h.backend.stats(), h.backend.listings(); stats != 0 || lists != 1 {
		t.Fatalf("the audit made %d stats and %d listings, want one listing and no stat", stats, lists)
	}
}

// EVERY WINDOW OF THE LISTING IS AUDITED, each reference once: a store holding
// more objects than a window is listed once a window, and what it lost or
// holds wrong in any of them is found — an object the engine did not put
// there taking no room in one.
func TestEveryWindowOfTheListingIsAudited(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.c.window = 3
	var all []objstore.Object
	for i := range 10 {
		all = append(all, h.file(fmt.Sprintf("f%02d.md", i), fmt.Sprintf("content %d", i)))
		// MINTED A MOMENT APART, so the keys' order is the files'.
		h.clock.advance(time.Millisecond)
	}
	if err := h.backend.Put(t.Context(), "elsewhere/x", strings.NewReader("x"), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(all, func(a, b objstore.Object) int { return a.Key.Compare(b.Key) })
	for _, i := range []int{1, 5, 9} {
		if err := h.store.Delete(t.Context(), all[i].Key); err != nil {
			t.Fatal(err)
		}
	}
	h.backend.Corrupt(all[4].Key.Name(), []byte("bent"))
	r, err := h.c.Audit(t.Context())
	if err != nil || r.Referenced != 10 || r.Missing != 3 || r.Damaged != 1 {
		t.Fatalf("Audit = %+v, %v; want ten files, three missing and one damaged", r, err)
	}
	if n := h.backend.listings(); n != 3 {
		t.Fatalf("seven held objects in windows of three were listed %d times, want 3", n)
	}
	h.backend.mu.Lock()
	defer h.backend.mu.Unlock()
	for _, i := range []int{1, 5, 9} {
		if n := h.backend.asks[all[i].Key.Name()]; n != 1 {
			t.Errorf("%s, in no window, was asked about by name %d times, want once", all[i].Key, n)
		}
	}
	if len(h.backend.asks) != 3 {
		t.Errorf("asked about by name: %v; want only the three no window held", h.backend.asks)
	}
}

// THE SECOND LOOK HOLDS AT MOST ITS LIMIT OF REFERENCES, oldest first, a
// reference or a range at a time: a push past it hands back the oldest entries
// until what is held fits — a range counting every reference it holds — and a
// drain hands back the rest in the order they came. An entry handed back is
// cleared from the array behind the queue, so the queue never keeps what it no
// longer counts reachable.
func TestTheSecondLookQueueHoldsAtMostItsLimit(t *testing.T) {
	t.Parallel()
	ref := func(i int) objstore.Reference { return objstore.Reference{NamedBy: fmt.Sprintf("ENG/%d.md", i)} }
	one := func(i int) lookAgain { return lookAgain{ref: ref(i)} }
	span := func(from, to int) lookAgain {
		l := lookAgain{refs: []objstore.Reference{}}
		for i := from; i <= to; i++ {
			l.refs = append(l.refs, ref(i))
		}
		return l
	}
	names := func(ls []lookAgain) []string {
		var out []string
		for _, l := range ls {
			if l.refs == nil {
				out = append(out, l.ref.NamedBy)
				continue
			}
			out = append(out, fmt.Sprintf("%s..%s", l.refs[0].NamedBy, l.refs[len(l.refs)-1].NamedBy))
		}
		return out
	}
	q := suspects{limit: 3}
	for i := 1; i <= 7; i++ {
		due := q.push(one(i))
		switch {
		case i <= 3 && len(due) != 0:
			t.Fatalf("push %d handed back %v while the queue had room", i, names(due))
		case i > 3 && !slices.Equal(names(due), names([]lookAgain{one(i - 3)})):
			t.Fatalf("push %d handed back %v, want the oldest, %q", i, names(due), ref(i-3).NamedBy)
		}
		if q.held > 3 {
			t.Fatalf("after push %d the queue holds %d, past its limit of 3", i, q.held)
		}
	}
	// A RANGE COUNTS EVERY REFERENCE IT HOLDS: two of them push out the two
	// oldest references, and the next reference the range itself.
	if due := q.push(span(10, 11)); !slices.Equal(names(due), []string{"ENG/5.md", "ENG/6.md"}) {
		t.Fatalf("a range of two handed back %v, want the two oldest references", names(due))
	}
	if due := q.push(one(12)); !slices.Equal(names(due), []string{"ENG/7.md"}) {
		t.Fatalf("a reference after the range handed back %v, want the oldest", names(due))
	}
	if due := q.push(one(13)); !slices.Equal(names(due), []string{"ENG/10.md..ENG/11.md"}) {
		t.Fatalf("a reference past the limit handed back %v, want the range", names(due))
	}
	for i, l := range q.queue[:cap(q.queue)] {
		if i >= q.head && i < len(q.queue) {
			continue // held
		}
		if l.refs != nil || l.ref != (objstore.Reference{}) {
			t.Fatalf("slot %d, handed back, is still reachable from the queue's array: %v", i, names([]lookAgain{l}))
		}
	}
	if got, want := names(q.drain()), []string{"ENG/12.md", "ENG/13.md"}; !slices.Equal(got, want) {
		t.Fatalf("drain = %v, want %v", got, want)
	}
	if rest := q.drain(); len(rest) != 0 || q.held != 0 {
		t.Fatalf("a drained queue still holds %v (%d)", names(rest), q.held)
	}
	// A LONE RANGE PAST THE LIMIT IS HELD rather than looked at again the
	// moment its first look failed, and the next push hands it back.
	if due := q.push(span(20, 24)); len(due) != 0 {
		t.Fatalf("a lone range past the limit was handed back at once: %v", names(due))
	}
	if due := q.push(one(25)); !slices.Equal(names(due), []string{"ENG/20.md..ENG/24.md"}) {
		t.Fatalf("the push after a range past the limit handed back %v, want the range", names(due))
	}
	// A LIMIT BELOW ONE HOLDS ONE rather than dropping what it is handed.
	unset := suspects{}
	if due := unset.push(one(1)); len(due) != 0 {
		t.Fatal("a queue with no limit set dropped the first reference it was handed")
	}
	if due := unset.push(one(2)); !slices.Equal(names(due), []string{"ENG/1.md"}) {
		t.Fatalf("a queue with no limit set handed back %v, want the first", names(due))
	}
}

// AN AUDIT TAKES ITS SECOND LOOK BEFORE ITS WALK ENDS ONCE IT HOLDS ITS BOUND:
// a store that lost everything must not have the audit hold the company's
// whole inventory until the end of the walk. Every reference whose first look
// found nothing still gets exactly one second look, the ones asked early
// included, and only those found nowhere are missing — whether the first look
// was a stat ("by_name") or the window of the listing it falls in ("listed").
//
// The walk's last first look waits for a second look to have been taken, so an
// audit that saved every second look for the end of its walk never finishes it:
// by name, the last first stat; listed, the last window's listing, which the
// walk needs before it can judge the references in it.
func TestTheAuditTakesItsSecondLookBeforeItsWalkEndsOnceItHoldsItsBound(t *testing.T) {
	t.Parallel()
	for _, arm := range []string{"by_name", "listed"} {
		t.Run(arm, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.c.suspectsHeld = 3
			var names []string
			for i := range 12 {
				o := h.file(fmt.Sprintf("late-%02d.md", i), fmt.Sprintf("late %d", i))
				if arm == "by_name" {
					h.backend.lagging[o.Key.Name()] = 1
				} else {
					h.backend.hide(o.Key.Name())
				}
				names = append(names, o.Key.Name())
			}
			for i := range 2 {
				o := h.file(fmt.Sprintf("lost-%d.md", i), fmt.Sprintf("lost %d", i))
				if err := h.store.Delete(t.Context(), o.Key); err != nil {
					t.Fatal(err)
				}
				names = append(names, o.Key.Name())
			}
			var (
				second    = make(chan struct{})
				sawSecond sync.Once
			)
			waitForASecondLook := func() {
				select {
				case <-second:
				case <-time.After(10 * time.Second):
					t.Error("the walk's last first look came with no second look taken: " +
						"every suspect was held for the end of the walk")
				}
			}
			// Each look by name answers which look of its object it is.
			nthSecond := 2
			if arm == "listed" {
				nthSecond = 1
				// LISTED OBJECTS AFTER THE SUSPECTS IN KEY ORDER, one to a
				// window, so the walk lists the store again for each: the
				// last listing is the walk's last first look.
				const listed = 3
				h.c.window = 1
				for i := range listed {
					h.clock.advance(time.Millisecond)
					h.file(fmt.Sprintf("listed-%d.md", i), fmt.Sprintf("listed %d", i))
				}
				h.backend.onList = func(_ context.Context, nth int) error {
					if nth == listed {
						waitForASecondLook()
					}
					return nil
				}
			}
			var (
				mu     sync.Mutex
				firsts int
			)
			h.backend.onStat = func(_ context.Context, _ string, nth int) error {
				if nth == nthSecond {
					sawSecond.Do(func() { close(second) })
					return nil
				}
				mu.Lock()
				firsts++
				last := firsts == len(names)
				mu.Unlock()
				if last {
					waitForASecondLook()
				}
				return nil
			}
			if arm == "by_name" {
				h.askByName()
			}

			r, err := h.c.Audit(t.Context())
			if err != nil || r.Missing != 2 || r.Found == nil || r.Found.Missing != 2 {
				t.Fatalf("Audit = %+v, %v; want the two lost files missing", r, err)
			}
			h.backend.mu.Lock()
			defer h.backend.mu.Unlock()
			for _, name := range names {
				if got := h.backend.asks[name]; got != nthSecond {
					t.Errorf("%s was asked about by name %d times, want %d: a first look and "+
						"exactly one second", name, got, nthSecond)
				}
			}
		})
	}
}

// WHAT AN AUDIT FOUND OUTLIVES AN AUDIT THAT FAILED AFTER IT: the attempt's
// error is reported, and its findings — which are floors — never replace the
// last whole ones, so a broker that stopped answering mid-audit never clears
// an alarm about files the store has lost.
func TestAFailedAuditKeepsTheLastFindings(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	lost := h.file("lost.md", "lost")
	if err := h.store.Delete(t.Context(), lost.Key); err != nil {
		t.Fatal(err)
	}
	first, err := h.c.Audit(t.Context())
	if err != nil || first.Found == nil || first.Found.Missing != 1 {
		t.Fatalf("the first audit = %+v, %v", first, err)
	}
	h.clock.advance(AuditInterval)
	// EVERY QUESTION FAILS, the second look included, and the store answers
	// none: a store that has gone away, never a file it refuses.
	h.backend.statErr = errors.New("the broker stopped answering")
	if _, err := h.c.Audit(t.Context()); !errors.Is(err, errStoreFailing) {
		t.Fatalf("an audit whose every question failed = %v, want errStoreFailing", err)
	}
	got := h.c.Status().Audit
	if got.Error == "" || !got.At.After(first.At) {
		t.Fatalf("the status = %+v; want the failed attempt reported", got)
	}
	if got.Found == nil || got.Found.Missing != 1 || !got.Found.At.Equal(first.At) ||
		len(got.Found.MissingFiles) != 1 || got.Found.MissingFiles[0].NamedBy != "ENG/lost.md" {
		t.Fatalf("the findings after a failed audit = %+v; want the last whole audit's", got.Found)
	}
}

// A NODE TAKING THE DUTY PICKS UP WHAT THE LAST HOLDER RECORDED, each half
// where it is newer than its own.
func TestRestoreKeepsTheNewerOfEachHalf(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	at := h.clock.Now()
	lost := MissingFile{Object: objstore.KeyAt(at), NamedBy: "ENG/lost.md", Finding: FindingMissing}
	h.c.Restore(Status{
		Collect: CollectionReport{Completed: true, Deleted: 3, At: at},
		Audit: AuditReport{Completed: true, Referenced: 9, Missing: 1, At: at,
			Found: &AuditFindings{At: at, Completed: true, Referenced: 9, Missing: 1,
				MissingFiles: []MissingFile{lost}}},
	})
	got := h.c.Status()
	if got.Collect.Deleted != 3 || got.Audit.Found == nil || got.Audit.Found.Missing != 1 ||
		got.Audit.Found.MissingFiles[0] != lost {
		t.Fatalf("the restored status = %+v", got)
	}
	// AN OLDER RECORD CHANGES NOTHING.
	h.c.Restore(Status{Collect: CollectionReport{At: at.Add(-time.Hour)},
		Audit: AuditReport{At: at.Add(-time.Hour), Found: &AuditFindings{At: at.Add(-time.Hour)}}})
	if again := h.c.Status(); again.Collect.Deleted != 3 || again.Audit.Referenced != 9 ||
		!again.Audit.At.Equal(at) || again.Audit.Found.Missing != 1 {
		t.Fatalf("an older record replaced a newer status: %+v", again)
	}
	// A FAILED ATTEMPT NEWER THAN THE FINDINGS keeps them.
	h.c.Restore(Status{Audit: AuditReport{At: at.Add(time.Hour), Error: "stopped"}})
	if again := h.c.Status(); again.Audit.Error != "stopped" || again.Audit.Found == nil ||
		again.Audit.Found.Missing != 1 {
		t.Fatalf("a newer failed attempt took the findings with it: %+v", again.Audit)
	}
}

// gate holds every call that passes it until it is opened, counting how many
// it holds at once.
type gate struct {
	open chan struct{}

	mu         sync.Mutex
	held, peak int
}

func newGate() *gate { return &gate{open: make(chan struct{})} }

// pass is a call arriving: counted, then held until the gate opens or ctx
// ends — whose error it answers.
func (g *gate) pass(ctx context.Context) error {
	g.mu.Lock()
	g.held++
	g.peak = max(g.peak, g.held)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.held--
		g.mu.Unlock()
	}()
	select {
	case <-g.open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// counts is how many calls the gate holds now, and the most it has held.
func (g *gate) counts() (held, peak int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held, g.peak
}

// waitHeld fails the test unless n calls are held at once within ten seconds.
func (g *gate) waitHeld(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		held, peak := g.counts()
		if held >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the gate never held %d calls at once (at most %d): the calls are not "+
				"made concurrently", n, peak)
		}
		time.Sleep(time.Millisecond)
	}
}

// THE AUDIT ASKS UP TO ITS BOUND AT ONCE, AND NEVER MORE. Asked one at a time,
// an audit of two million objects on S3 is most of a day; asked without a
// bound, it floods the store it is auditing. So with its first looks by name,
// and with its second looks of everything a listing left out.
func TestTheAuditAsksAtMostItsBoundAtOnce(t *testing.T) {
	t.Parallel()
	for _, arm := range []string{"by_name", "second_looks"} {
		t.Run(arm, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			for i := range 3 * auditConcurrency {
				o := h.file(fmt.Sprintf("%03d.md", i), fmt.Sprintf("object %d", i))
				if arm == "second_looks" {
					h.backend.hide(o.Key.Name())
				}
			}
			if arm == "by_name" {
				h.askByName()
			}
			g := newGate()
			h.backend.onStat = func(ctx context.Context, _ string, _ int) error { return g.pass(ctx) }
			type result struct {
				r   AuditReport
				err error
			}
			done := make(chan result, 1)
			go func() {
				r, err := h.c.Audit(t.Context())
				done <- result{r, err}
			}()
			g.waitHeld(t, auditConcurrency)
			// AND NO FURTHER while every one of them is held: a question
			// waits for one to be answered before it is asked.
			time.Sleep(50 * time.Millisecond)
			if _, peak := g.counts(); peak != auditConcurrency {
				t.Fatalf("the audit asked %d questions at once, past its bound of %d", peak, auditConcurrency)
			}
			close(g.open)
			got := <-done
			if got.err != nil || !got.r.Completed || got.r.Referenced != 3*auditConcurrency || got.r.Missing != 0 {
				t.Fatalf("Audit = %+v, %v; want every object asked about and held", got.r, got.err)
			}
			if held, peak := g.counts(); held != 0 || peak != auditConcurrency {
				t.Fatalf("after the audit the gate holds %d and held at most %d; want nothing left "+
					"asking and the bound reached, never passed", held, peak)
			}
		})
	}
}

// A COLLECTION DELETES UP TO ITS BOUND AT ONCE, AND NEVER MORE, while its
// listing goes on — and every deletion it began is counted.
func TestACollectionDeletesAtMostItsBoundAtOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var orphans []objstore.Key
	for i := range 2 * JudgeBatch {
		orphans = append(orphans, h.put(fmt.Sprintf("orphan %d", i)))
	}
	h.clock.advance(PendingGrace + time.Hour)
	g := newGate()
	h.backend.onDelete = func(ctx context.Context, _ string) error { return g.pass(ctx) }
	type result struct {
		r   CollectionReport
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := h.c.Collect(t.Context())
		done <- result{r, err}
	}()
	g.waitHeld(t, deleteConcurrency)
	time.Sleep(50 * time.Millisecond)
	if _, peak := g.counts(); peak != deleteConcurrency {
		t.Fatalf("the collection had %d deletions in flight, past its bound of %d", peak, deleteConcurrency)
	}
	close(g.open)
	got := <-done
	if got.err != nil || !got.r.Completed || got.r.Deleted != len(orphans) {
		t.Fatalf("Collect = %+v, %v; want all %d orphans deleted", got.r, got.err, len(orphans))
	}
	if held, peak := g.counts(); held != 0 || peak != deleteConcurrency {
		t.Fatalf("after the collection the gate holds %d and held at most %d", held, peak)
	}
	for _, k := range orphans {
		if h.held(k) {
			t.Fatalf("%s survived a collection that reported it deleted", k)
		}
	}
}

// THE FILES A STATUS SHOWS ARE THE FIRST BY NAME, whatever order the audit
// found them in: its questions run at once, so the order they are answered
// in is chance, and a list chosen by it would name a different hundred each
// day for the same lost files.
func TestTheFilesShownAreTheFirstByName(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	lost := MissingShown + 50
	// CREATED IN REVERSE, so the keys' order — which is their minting
	// order — and the files' order disagree.
	for i := lost - 1; i >= 0; i-- {
		o := h.file(fmt.Sprintf("f%03d.md", i), fmt.Sprintf("lost %d", i))
		if err := h.store.Delete(t.Context(), o.Key); err != nil {
			t.Fatal(err)
		}
	}
	r, err := h.c.Audit(t.Context())
	if err != nil || r.Missing != lost || r.Found == nil {
		t.Fatalf("Audit = %+v, %v; want %d missing", r, err, lost)
	}
	var got, want []string
	for _, f := range r.Found.MissingFiles {
		got = append(got, f.NamedBy)
	}
	for i := range MissingShown {
		want = append(want, fmt.Sprintf("ENG/f%03d.md", i))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the files shown are %v, want the first %d by name: %v", got, MissingShown, want)
	}
}

// A PASS ITS CALLER STOPPED SAYS WHY — the engine stops one when its duty
// moves to another node — rather than reporting "context canceled", and
// leaves no call to the store running behind it: an audit stopped in its
// listing, and one stopped in a second look by name, as a collection stopped
// in a deletion.
func TestAStoppedPassSaysWhy(t *testing.T) {
	t.Parallel()
	for _, pass := range []string{"audit", "audit_second_look", "collection"} {
		t.Run(pass, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			g := newGate()
			a := h.file("a.md", "a file")
			run := func(ctx context.Context) (string, error) {
				r, err := h.c.Audit(ctx)
				return r.Error, err
			}
			switch pass {
			case "audit":
				h.backend.onList = func(ctx context.Context, _ int) error { return g.pass(ctx) }
			case "audit_second_look":
				h.backend.hide(a.Key.Name())
				h.backend.onStat = func(ctx context.Context, _ string, _ int) error { return g.pass(ctx) }
			case "collection":
				h.put("an orphan")
				h.clock.advance(PendingGrace + time.Hour)
				h.backend.onDelete = func(ctx context.Context, _ string) error { return g.pass(ctx) }
				run = func(ctx context.Context) (string, error) {
					r, err := h.c.Collect(ctx)
					return r.Error, err
				}
			}
			why := errors.New("another node holds the duty now")
			ctx, stop := context.WithCancelCause(t.Context())
			type result struct {
				reported string
				err      error
			}
			done := make(chan result, 1)
			go func() {
				reported, err := run(ctx)
				done <- result{reported, err}
			}()
			g.waitHeld(t, 1)
			stop(why)
			got := <-done
			if !errors.Is(got.err, why) || !strings.Contains(got.reported, why.Error()) {
				t.Fatalf("a stopped %s returned %v and reported %q; want it to say why it "+
					"was stopped", pass, got.err, got.reported)
			}
			if held, _ := g.counts(); held != 0 {
				t.Fatalf("%d calls to the store outlived the %s", held, pass)
			}
		})
	}
}

// AN AUDIT STOPPED BEFORE ITS SECOND LOOKS HAS NOT RUN TO ITS END, although no
// question it asked failed: the one first look ANSWERS — the listing that left
// the object out, or a "not found" by name, neither needing the context — so
// nothing is in flight when the stop lands, and the drain stops between two
// questions rather than inside one. Reported as complete, its floor counts
// would replace the last whole audit's findings and clear the alarm; the
// engine stops an audit exactly this way when its duty moves.
func TestAnAuditStoppedBeforeItsSecondLooksHasNotRunToItsEnd(t *testing.T) {
	t.Parallel()
	for _, arm := range []string{"listed", "by_name"} {
		t.Run(arm, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			late := h.file("late.md", "an object the store lists late")
			ctx, stop := context.WithCancelCause(t.Context())
			defer stop(nil)
			why := errors.New("the duty moved")
			if arm == "listed" {
				h.backend.hide(late.Key.Name())
				h.backend.onList = func(context.Context, int) error {
					stop(why)
					return nil
				}
			} else {
				h.askByName()
				h.backend.mu.Lock()
				h.backend.lagging[late.Key.Name()] = 1
				h.backend.mu.Unlock()
				h.backend.onStat = func(_ context.Context, _ string, nth int) error {
					if nth == 1 {
						stop(why)
					}
					return nil
				}
			}
			r, err := h.c.Audit(ctx)
			if !errors.Is(err, why) {
				t.Fatalf("an audit stopped before its second looks returned %v, want it to say "+
					"it was stopped by %v", err, why)
			}
			if r.Completed || r.Found != nil || !strings.Contains(r.Error, "the audit was stopped") {
				t.Fatalf("an audit stopped before its second looks reported %+v; want it "+
					"incomplete, with no findings of its own and the stop as its error", r)
			}
			if found := h.c.Status().Audit.Found; found != nil {
				t.Fatalf("an audit stopped before its second looks recorded findings %+v", found)
			}
			if n := h.backend.stats(); arm == "listed" && n != 0 {
				t.Fatalf("an audit stopped before its second looks asked %d questions by name", n)
			}
		})
	}
}

// A STORE THAT FAILS QUESTION AFTER QUESTION STOPS THE AUDIT, with its own
// error, once it has failed a window of them with none answered between — its
// listing, and then the questions by name a failed listing turns the audit to
// — and the audit returns only once every question it asked has ended: a
// question to the store never outlives the pass that asked it. The findings
// the last audit made stand, and none of the failed questions is taken for a
// finding.
func TestAStoreThatFailsQuestionAfterQuestionStopsTheAudit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	lost := h.file("lost.md", "lost")
	if err := h.store.Delete(t.Context(), lost.Key); err != nil {
		t.Fatal(err)
	}
	if r, err := h.c.Audit(t.Context()); err != nil || r.Found == nil || r.Found.Missing != 1 {
		t.Fatalf("the first audit = %+v, %v", r, err)
	}
	for i := range 3 * auditConcurrency {
		h.file(fmt.Sprintf("%03d.md", i), fmt.Sprintf("object %d", i))
	}
	broke := errors.New("the store stopped answering")
	h.backend.mu.Lock()
	h.backend.listErr = broke
	h.backend.mu.Unlock()
	var (
		mu       sync.Mutex
		asked    int
		inFlight atomic.Int64
	)
	h.backend.onStat = func(ctx context.Context, _ string, _ int) error {
		mu.Lock()
		asked++
		n := asked
		mu.Unlock()
		if n <= 5 {
			return nil
		}
		inFlight.Add(1)
		defer inFlight.Add(-1)
		// A REQUEST TAKES A MOMENT TO FAIL, so the questions beside the
		// one that stops the audit are still out when it does — and an
		// audit that returned without waiting for them is caught with them
		// running.
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			time.Sleep(20 * time.Millisecond)
			return ctx.Err()
		}
		return broke
	}
	h.clock.advance(AuditInterval)
	r, err := h.c.Audit(t.Context())
	if !errors.Is(err, broke) || !errors.Is(err, errStoreFailing) || r.Completed ||
		!strings.Contains(r.Error, broke.Error()) {
		t.Fatalf("Audit = %+v, %v; want it failed as a failing store, with the store's own error", r, err)
	}
	if n := inFlight.Load(); n != 0 {
		t.Fatalf("%d questions were still running when the audit returned", n)
	}
	mu.Lock()
	n := asked
	mu.Unlock()
	if n > 5+failedInARow+auditConcurrency {
		t.Fatalf("a store that answered nothing was asked %d questions; want the audit stopped "+
			"within a window of its run of %d", n, failedInARow)
	}
	if r.Found == nil || r.Found.Missing != 1 || r.Found.Unanswered != 0 ||
		r.Found.MissingFiles[0].NamedBy != "ENG/lost.md" {
		t.Fatalf("the findings after a failed audit = %+v; want the last whole audit's", r.Found)
	}
}

// ONE OBJECT THE STORE KEEPS FAILING IS UNANSWERED, AND THE AUDIT RUNS TO ITS
// END: asked by name, set aside, asked again after the walk, and named — or,
// left out of a listing, asked by name once and named. Failing the audit
// instead never named the file, and the engine's retry ten minutes later asked
// the store about every object before it again — every attempt of every day —
// while the alarm kept the last audit to have finished. The store answers
// every other question, which is what makes the failure the object's.
func TestAnObjectTheStoreKeepsFailingIsUnanswered(t *testing.T) {
	t.Parallel()
	for _, arm := range []string{"by_name", "listed"} {
		t.Run(arm, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			var bad objstore.Object
			for i := range 2*auditConcurrency + 3 {
				o := h.file(fmt.Sprintf("%03d.md", i), fmt.Sprintf("object %d", i))
				if i == auditConcurrency {
					bad = o
				}
			}
			asks := 1
			if arm == "by_name" {
				h.askByName()
				asks = 2
			} else {
				h.backend.hide(bad.Key.Name())
			}
			refused := errors.New("InternalError: we encountered an internal error, please try again")
			h.backend.onStat = func(_ context.Context, name string, _ int) error {
				if name == bad.Key.Name() {
					return refused
				}
				return nil
			}
			r, err := h.c.Audit(t.Context())
			if err != nil || !r.Completed || r.Unanswered != 1 || r.Missing != 0 || r.Damaged != 0 {
				t.Fatalf("Audit = %+v, %v; want it complete with the one file unanswered", r, err)
			}
			want := []MissingFile{{Object: bad.Key, NamedBy: fmt.Sprintf("ENG/%03d.md", auditConcurrency),
				Finding: FindingUnanswered}}
			if r.Found == nil || r.Found.Unanswered != 1 || r.Found.Lost() != 1 ||
				!slices.Equal(r.Found.MissingFiles, want) {
				t.Fatalf("the findings = %+v, want %+v", r.Found, want)
			}
			h.backend.mu.Lock()
			defer h.backend.mu.Unlock()
			for name, n := range h.backend.asks {
				switch {
				case name == bad.Key.Name() && n != asks:
					t.Errorf("the failing object was asked about %d times, want %d", n, asks)
				case name != bad.Key.Name() && n > asks:
					t.Errorf("%s was asked about %d times; nothing is walked again", name, n)
				}
			}
		})
	}
}

// A STORE THAT STOPS ANSWERING AS THE AUDIT ENDS FAILS IT, rather than having
// every second look it failed then called unanswered: the store is asked once
// more, about an object it answered for, and only an answer there says the
// failures were the objects'. A store that answered nothing all audit is
// failing too, however few the questions. So whether the first looks were by
// name or from the listing — whose answer settles the objects it holds.
func TestAStoreThatStopsAnsweringAsTheAuditEndsFailsIt(t *testing.T) {
	t.Parallel()
	for _, arm := range []string{"by_name", "listed"} {
		t.Run(arm, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.file("kept.md", "kept")
			late := h.file("late.md", "an object the store has not caught up with")
			// The look by name that is the late object's second.
			second := 1
			if arm == "by_name" {
				h.askByName()
				second = 2
			} else {
				h.backend.hide(late.Key.Name())
			}
			setLate := func() {
				h.backend.mu.Lock()
				defer h.backend.mu.Unlock()
				clear(h.backend.asks)
				if arm == "by_name" {
					h.backend.lagging[late.Key.Name()] = 1
				}
			}
			setLate()
			broke := errors.New("the store went away")
			var down atomic.Bool
			h.backend.onStat = func(_ context.Context, name string, nth int) error {
				if name == late.Key.Name() && nth == second {
					down.Store(true)
				}
				if down.Load() {
					return broke
				}
				return nil
			}
			r, err := h.c.Audit(t.Context())
			if !errors.Is(err, errStoreFailing) || r.Completed || r.Found != nil {
				t.Fatalf("Audit = %+v, %v; want it failed as a failing store, finding nothing", r, err)
			}

			// AND THE SAME LAST SECOND LOOK, FAILED BY A STORE STILL
			// ANSWERING, IS THE OBJECT'S: the question after it is answered.
			setLate()
			down.Store(false)
			h.backend.onStat = func(_ context.Context, name string, nth int) error {
				if name == late.Key.Name() && nth == second {
					return broke
				}
				return nil
			}
			r, err = h.c.Audit(t.Context())
			if err != nil || r.Unanswered != 1 || r.Found == nil || r.Found.Unanswered != 1 {
				t.Fatalf("Audit = %+v, %v; want the one file unanswered", r, err)
			}
		})
	}
}

// A DELETION THAT FAILS STOPS THE COLLECTION WITH ITS OWN ERROR — never the
// cancellation it caused — and the pass returns only once every deletion it
// began has ended, counting exactly those that took effect.
//
// THE COUNT IS JUDGED AGAINST THE STORE, never against a number: a deletion
// the store let through before the failure can still reach the twin after the
// failure cancelled the pass, and fail there, so how many of the nineteen
// before it take effect is the scheduler's to decide. What is not is that the
// report counts every one that did and none that did not, and that nothing
// after the failure is deleted.
func TestAFailedDeletionStopsTheCollection(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var orphans []objstore.Key
	for i := range 2 * JudgeBatch {
		orphans = append(orphans, h.put(fmt.Sprintf("orphan %d", i)))
	}
	h.clock.advance(PendingGrace + time.Hour)
	broke := errors.New("the store refused a deletion")
	var (
		mu       sync.Mutex
		calls    int
		inFlight atomic.Int64
	)
	h.backend.onDelete = func(ctx context.Context, _ string) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch {
		case n < 20:
			return nil
		case n == 20:
			return broke
		}
		inFlight.Add(1)
		defer inFlight.Add(-1)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		return ctx.Err()
	}
	r, err := h.c.Collect(t.Context())
	if !errors.Is(err, broke) || errors.Is(err, context.Canceled) || r.Completed {
		t.Fatalf("Collect = %+v, %v; want it failed with the deletion's own error alone", r, err)
	}
	if n := inFlight.Load(); n != 0 {
		t.Fatalf("%d deletions were still running when the collection returned", n)
	}
	gone := 0
	for _, k := range orphans {
		if !h.held(k) {
			gone++
		}
	}
	if r.Deleted != gone || gone >= 20 {
		t.Fatalf("the collection reported %d deleted and the store lost %d; want the report "+
			"to count exactly the deletions that took effect, all of them before the 20th", r.Deleted, gone)
	}
}

// EVERY REFERENCE IS LOOKED AT ONCE, AND A SUSPECT EXACTLY TWICE, however the
// questions interleave: nothing held asked about again, nothing called missing
// on one answer, nothing damaged counted as missing. By name, a reference's
// first look is a stat; from the listing, read here across windows, it is the
// window its key falls in, and only what no window held is asked by name.
func TestEveryReferenceIsAskedOnceAndASuspectTwice(t *testing.T) {
	t.Parallel()
	for _, arm := range []string{"by_name", "listed"} {
		t.Run(arm, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			const total, deleted, corrupted, late = 1507, 37, 13, 5
			// suspects is every name whose first look found nothing, which
			// gets one more by name.
			suspects := map[string]bool{}
			for i := range total {
				o := h.file(fmt.Sprintf("%04d.md", i), fmt.Sprintf("object %d", i))
				switch {
				case i < deleted:
					if err := h.store.Delete(t.Context(), o.Key); err != nil {
						t.Fatal(err)
					}
					suspects[o.Key.Name()] = true
				case i < deleted+corrupted:
					h.backend.Corrupt(o.Key.Name(), []byte("rot"))
				case i < deleted+corrupted+late:
					if arm == "by_name" {
						h.backend.lagging[o.Key.Name()] = 1
					} else {
						h.backend.hide(o.Key.Name())
					}
					suspects[o.Key.Name()] = true
				}
			}
			const window = 100
			if arm == "by_name" {
				h.askByName()
			} else {
				h.c.window = window
			}
			r, err := h.c.Audit(t.Context())
			if err != nil || r.Referenced != total || r.Missing != deleted || r.Damaged != corrupted {
				t.Fatalf("Audit = %+v, %v; want %d asked, %d missing and %d damaged", r, err, total, deleted, corrupted)
			}
			h.backend.mu.Lock()
			defer h.backend.mu.Unlock()
			if arm == "listed" {
				listed := total - deleted - late
				if want := (listed + window - 1) / window; h.backend.lists != want {
					t.Errorf("%d held objects in windows of %d were listed %d times, want %d",
						listed, window, h.backend.lists, want)
				}
				if len(h.backend.asks) != len(suspects) {
					t.Fatalf("%d names asked about by name, want the %d no window held",
						len(h.backend.asks), len(suspects))
				}
				for name, n := range h.backend.asks {
					if !suspects[name] || n != 1 {
						t.Errorf("%s asked about by name %d times; want once, and only for a suspect", name, n)
					}
				}
				return
			}
			if len(h.backend.asks) != total {
				t.Fatalf("%d names asked about, want %d", len(h.backend.asks), total)
			}
			for name, n := range h.backend.asks {
				want := 1
				if suspects[name] {
					want = 2
				}
				if n != want {
					t.Errorf("%s asked about %d times, want %d", name, n, want)
				}
			}
		})
	}
}

// THE AUDIT ASKS NO MORE AT ONCE THAN THE TRANSPORT KEEPS WARM, and deletes
// fewer at once than it asks. On S3 an audit past the shared transport's warm
// connections to one host pays a fresh TLS handshake for every question over
// the pool; a deletion is a WRITE, on the share of the bucket's request rate
// the company's own uploads spend.
func TestTheAuditAsksNoMoreAtOnceThanTheTransportKeepsWarm(t *testing.T) {
	t.Parallel()
	if auditConcurrency > httpx.MaxIdleConnsPerHost {
		t.Errorf("the audit asks %d at once and the transport keeps %d connections warm per host",
			auditConcurrency, httpx.MaxIdleConnsPerHost)
	}
	if deleteConcurrency >= auditConcurrency {
		t.Errorf("a collection deletes %d at once, not fewer than the %d questions an audit asks",
			deleteConcurrency, auditConcurrency)
	}
}
