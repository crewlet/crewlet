package collect

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeSource is one domain's references, held in memory.
type fakeSource struct {
	mu         sync.Mutex
	named      map[objstore.Key]bool
	incomplete bool
	barriers   int
}

func (s *fakeSource) Name() string { return "tracker" }

func (s *fakeSource) Barrier(context.Context) (statelog.Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.barriers++
	return statelog.Position{}, nil
}

func (s *fakeSource) Referenced(_ context.Context, among []objstore.Key,
	_ statelog.Position) (map[objstore.Key]struct{}, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[objstore.Key]struct{}{}
	for _, k := range among {
		if s.named[k] {
			out[k] = struct{}{}
		}
	}
	return out, !s.incomplete, nil
}

func (s *fakeSource) Each(_ context.Context, _ statelog.Position, visit func(objstore.Key) error) (bool, error) {
	s.mu.Lock()
	named := make([]objstore.Key, 0, len(s.named))
	for k := range s.named {
		named = append(named, k)
	}
	incomplete := s.incomplete
	s.mu.Unlock()
	for _, k := range named {
		if err := visit(k); err != nil {
			return false, err
		}
	}
	return !incomplete, nil
}

func (s *fakeSource) name(k objstore.Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.named[k] = true
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

type harness struct {
	t       *testing.T
	clock   *clock
	backend *memobj.Backend
	store   *objstore.Store
	source  *fakeSource
	c       *Collector
}

// newHarness is a collector over an in-memory store whose writes are dated,
// and whose keys are minted, at one settable clock.
func newHarness(t *testing.T) *harness {
	t.Helper()
	clk := &clock{now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	backend := memobj.NewAt(clk.Now)
	store, err := objstore.NewStoreAt(backend, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{named: map[objstore.Key]bool{}}
	c, err := New(Options{Store: store, References: References{src}, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, clock: clk, backend: backend, store: store, source: src, c: c}
}

func (h *harness) put(content string) objstore.Key {
	h.t.Helper()
	o, err := h.store.Put(h.t.Context(), strings.NewReader(content), 1<<20, objstore.PutMeta{})
	if err != nil {
		h.t.Fatal(err)
	}
	return o.Key
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

func (h *harness) held(k objstore.Key) bool {
	h.t.Helper()
	_, err := h.store.Stat(h.t.Context(), k)
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
	named := h.put("a file's bytes")
	h.source.name(named)

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
// old. That includes the chunks an earlier build stored, which wait for the
// chunk era to be over, and a bare UUID outside the engine's namespace, which
// is what another application sharing an S3 bucket under an empty prefix
// names its own objects with.
func TestANameThatIsNotAKeyIsLeftAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := objstore.KeyAt(h.clock.Now())
	foreign := []string{
		"somebody-elses.txt",
		string(objstore.HashOf([]byte("a chunk an earlier build stored"))),
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

// THE BARRIER IS TAKEN BEFORE THE STORE IS LISTED, so the estate a pass judges
// by is at least as new as the moment it began.
func TestAPassPinsTheEstateFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if _, err := h.c.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h.source.barriers != 1 {
		t.Fatalf("%d barriers, want one per pass", h.source.barriers)
	}
}

// MORE OBJECTS THAN ONE BATCH are all judged, the last partial batch
// included.
func TestEveryBatchIsJudged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var keep []objstore.Key
	for i := range judgeBatch*2 + 7 {
		k := h.put("object " + time.Duration(i).String())
		if i%3 == 0 {
			h.source.name(k)
			keep = append(keep, k)
		}
	}
	h.clock.advance(PendingGrace + time.Hour)
	r, err := h.c.Collect(t.Context())
	if err != nil || !r.Completed {
		t.Fatalf("Collect = %+v, %v", r, err)
	}
	if r.Listed != judgeBatch*2+7 || r.Deleted+r.Referenced != r.Listed || r.Referenced != len(keep) {
		t.Fatalf("the pass judged %+v; want every object judged and %d kept", r, len(keep))
	}
	for _, k := range keep {
		if !h.held(k) {
			t.Fatalf("a named object past the first batch was deleted")
		}
	}
}

// THE AUDIT COUNTS EVERY OBJECT THE ESTATE NAMES THAT THE STORE DOES NOT HOLD,
// and names them.
func TestTheAuditCountsWhatTheStoreLost(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	kept := h.put("kept")
	lost := h.put("lost")
	h.source.name(kept)
	h.source.name(lost)
	if err := h.store.Delete(t.Context(), lost); err != nil {
		t.Fatal(err)
	}
	r, err := h.c.Audit(t.Context())
	if err != nil || !r.Completed || r.Referenced != 2 || r.Missing != 1 ||
		len(r.MissingObjects) != 1 || r.MissingObjects[0] != lost {
		t.Fatalf("Audit = %+v, %v; want the lost object counted and named", r, err)
	}
	if got := h.c.Status().Audit.Missing; got != 1 {
		t.Fatalf("the status says %d missing", got)
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
