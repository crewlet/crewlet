package collect

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeSource is one domain's references, held in memory.
type fakeSource struct {
	mu         sync.Mutex
	named      map[objstore.Hash]bool
	incomplete bool
	barriers   int
	// onRead runs at each reference read, for the race cases.
	onRead func()
}

func (s *fakeSource) Name() string { return "tracker" }

func (s *fakeSource) Barrier(context.Context) (statelog.Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.barriers++
	return statelog.Position{}, nil
}

func (s *fakeSource) Referenced(_ context.Context, among []objstore.Hash,
	_ statelog.Position) (map[objstore.Hash]struct{}, bool, error) {
	if s.onRead != nil {
		s.onRead()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[objstore.Hash]struct{}{}
	for _, h := range among {
		if s.named[h] {
			out[h] = struct{}{}
		}
	}
	return out, !s.incomplete, nil
}

func (s *fakeSource) Each(_ context.Context, _ statelog.Position, visit func(objstore.Hash) error) (bool, error) {
	s.mu.Lock()
	named := make([]objstore.Hash, 0, len(s.named))
	for h := range s.named {
		named = append(named, h)
	}
	incomplete := s.incomplete
	s.mu.Unlock()
	for _, h := range named {
		if err := visit(h); err != nil {
			return false, err
		}
	}
	return !incomplete, nil
}

func (s *fakeSource) name(h objstore.Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.named[h] = true
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

func newHarness(t *testing.T) *harness {
	t.Helper()
	clk := &clock{now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	backend := memobj.NewAt(clk.Now)
	store, err := objstore.NewStore(backend, coordmem.NewFleet(), "writer")
	if err != nil {
		t.Fatal(err)
	}
	collectorStore, err := objstore.NewStore(backend, nil, "collector")
	if err == nil || collectorStore != nil {
		t.Fatal("a store without locks was built")
	}
	src := &fakeSource{named: map[objstore.Hash]bool{}}
	c, err := New(Options{Store: store, References: References{src}, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, clock: clk, backend: backend, store: store, source: src, c: c}
}

func (h *harness) put(content string) objstore.Hash {
	h.t.Helper()
	data := []byte(content)
	hash := objstore.HashOf(data)
	if err := h.store.Put(h.t.Context(), hash, data); err != nil {
		h.t.Fatal(err)
	}
	return hash
}

func (h *harness) held(hash objstore.Hash) bool {
	h.t.Helper()
	_, err := h.backend.Stat(h.t.Context(), hash)
	if errors.Is(err, objstore.ErrNotFound) {
		return false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return true
}

// A CHUNK NOTHING NAMES GOES ONCE IT IS PAST THE GRACE, and not before: every
// chunk is unreferenced from its upload until its file's record lands.
func TestAnUnreferencedChunkGoesOnlyAfterTheGrace(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	orphan := h.put("an upload whose record never came")
	named := h.put("a file's chunk")
	h.source.name(named)

	h.clock.advance(PendingGrace - time.Minute)
	if r, err := h.c.Collect(t.Context()); err != nil || r.Deleted != 0 || !r.Completed {
		t.Fatalf("a pass inside the grace = %+v, %v", r, err)
	}
	if !h.held(orphan) {
		t.Fatal("a chunk inside its grace was deleted")
	}
	h.clock.advance(2 * time.Minute)
	r, err := h.c.Collect(t.Context())
	if err != nil || r.Deleted != 1 || r.Referenced != 1 || !r.Completed {
		t.Fatalf("a pass past the grace = %+v, %v", r, err)
	}
	if h.held(orphan) {
		t.Fatal("an unreferenced chunk past its grace was kept")
	}
	if !h.held(named) {
		t.Fatal("a chunk a file names was deleted")
	}
}

// AN ESTATE THAT IS NOT COMPLETE DELETES NOTHING: a record this node could not
// apply may be the one naming the chunk.
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
		t.Fatal("a chunk was deleted on the word of an incomplete estate")
	}
}

// A CHUNK RE-PUT WHILE THE COLLECTOR JUDGES IT IS KEPT: a new file re-using an
// old chunk makes it young before naming it, and the delete re-reads the age
// under the lock the re-put takes.
func TestAChunkReUsedDuringThePassIsKept(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	data := []byte("re-used by a new file")
	reused := objstore.HashOf(data)
	if err := h.store.Put(t.Context(), reused, data); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(PendingGrace + time.Hour)
	// The new file re-puts the chunk AFTER the pass listed it old and
	// before it decided — and its record has not landed yet.
	h.source.onRead = func() {
		if err := h.store.Put(context.WithoutCancel(t.Context()), reused, data); err != nil {
			t.Error(err)
		}
	}
	r, err := h.c.Collect(t.Context())
	if err != nil || r.Deleted != 0 || r.Refreshed != 1 {
		t.Fatalf("the pass = %+v, %v; want the re-put chunk kept", r, err)
	}
	if !h.held(reused) {
		t.Fatal("a chunk a new file had just re-put was deleted")
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

// MORE CHUNKS THAN ONE BATCH are all judged, the last partial batch included.
func TestEveryBatchIsJudged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var keep []objstore.Hash
	for i := range judgeBatch*2 + 7 {
		hash := h.put("chunk " + string(rune('a'+i%26)) + time.Duration(i).String())
		if i%3 == 0 {
			h.source.name(hash)
			keep = append(keep, hash)
		}
	}
	h.clock.advance(PendingGrace + time.Hour)
	r, err := h.c.Collect(t.Context())
	if err != nil || !r.Completed {
		t.Fatalf("Collect = %+v, %v", r, err)
	}
	if r.Listed != judgeBatch*2+7 || r.Deleted+r.Referenced != r.Listed || r.Referenced != len(keep) {
		t.Fatalf("the pass judged %+v; want every chunk judged and %d kept", r, len(keep))
	}
	for _, hash := range keep {
		if !h.held(hash) {
			t.Fatalf("a named chunk past the first batch was deleted")
		}
	}
}

// THE AUDIT COUNTS EVERY CHUNK THE ESTATE NAMES THAT THE STORE DOES NOT HOLD,
// and names them.
func TestTheAuditCountsWhatTheStoreLost(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	kept := h.put("kept")
	lost := h.put("lost")
	h.source.name(kept)
	h.source.name(lost)
	if err := h.backend.Delete(t.Context(), lost); err != nil {
		t.Fatal(err)
	}
	r, err := h.c.Audit(t.Context())
	if err != nil || !r.Completed || r.Referenced != 2 || r.Missing != 1 ||
		len(r.MissingChunks) != 1 || r.MissingChunks[0] != lost {
		t.Fatalf("Audit = %+v, %v; want the lost chunk counted and named", r, err)
	}
	if got := h.c.Status().Audit.Missing; got != 1 {
		t.Fatalf("the status says %d missing", got)
	}
}

// A COLLECTOR WITH NOTHING TO READ REFERENCES FROM IS REFUSED: it would read
// every chunk as unreferenced and delete the company's files a day later.
func TestACollectorWithNoReferencesIsRefused(t *testing.T) {
	t.Parallel()
	store, err := objstore.NewStore(memobj.New(), coordmem.NewFleet(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Store: store}); err == nil {
		t.Fatal("a collector with no references was built")
	}
}
