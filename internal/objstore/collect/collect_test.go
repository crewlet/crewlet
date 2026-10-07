package collect

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
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
	mu    sync.Mutex
	named map[objstore.Key]Reference
	// incomplete is whether every read reports itself incomplete, and
	// incompleteAfter how many judged batches answer complete first.
	incomplete      bool
	incompleteAfter int
	judged          int
	barriers        int
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

func (s *fakeSource) Each(_ context.Context, _ statelog.Position, visit func(Reference) error) (bool, error) {
	s.mu.Lock()
	named := slices.Collect(maps.Values(s.named))
	incomplete := s.incomplete
	s.mu.Unlock()
	for _, ref := range named {
		if err := visit(ref); err != nil {
			return false, err
		}
	}
	return !incomplete, nil
}

// name records a row naming o, owned by the file at path.
func (s *fakeSource) name(o objstore.Object, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.named[o.Key] = Reference{Object: o, NamedBy: "ENG/" + path}
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
type backend struct {
	*memobj.Backend

	mu         sync.Mutex
	pending    []objstore.Pending
	pendingErr error
	// listTail is what a listing answers once it has handed on every
	// object — an S3 gateway's ErrUndated, say.
	listTail error
	// lagging is how many more times a stat of a name answers "not found"
	// before it answers what the twin holds.
	lagging map[string]int
	statErr error
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
	if err := b.Backend.List(ctx, visit); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.listTail
}

func (b *backend) Abandon(_ context.Context, p objstore.Pending) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = slices.DeleteFunc(b.pending, func(q objstore.Pending) bool { return q.ID == p.ID })
	return nil
}

func (b *backend) Stat(ctx context.Context, name string) (objstore.Info, error) {
	b.mu.Lock()
	failure := b.statErr
	lag := b.lagging[name]
	if lag > 0 {
		b.lagging[name] = lag - 1
	}
	b.mu.Unlock()
	switch {
	case failure != nil:
		return objstore.Info{}, failure
	case lag > 0:
		return objstore.Info{}, objstore.ErrNotFound
	}
	return b.Backend.Stat(ctx, name)
}

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
	b := &backend{Backend: memobj.NewAt(clk.Now), lagging: map[string]int{}}
	store, err := objstore.NewStoreAt(b, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{named: map[objstore.Key]Reference{}}
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
	if r.Listed != judgeBatch*2+7 || r.Deleted+r.Referenced != r.Listed || r.Referenced != len(keep) {
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
// never when somebody else's name does. Every upload in flight is pending too, which is what the grace is for.
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
	for i := range judgeBatch + 10 {
		h.put(fmt.Sprintf("orphan %d", i))
	}
	h.source.incompleteAfter = 1
	h.clock.advance(PendingGrace + time.Hour)
	r, err := h.c.Collect(t.Context())
	if err != nil || r.Deleted != judgeBatch || r.Skipped == "" || r.Completed {
		t.Fatalf("the pass = %+v, %v; want one batch deleted and why it stopped", r, err)
	}
	if got := h.c.Status().Collect; got.Deleted != judgeBatch || got.Skipped == "" {
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
		{Object: bent.Key, NamedBy: "ENG/bent.md", Damaged: true},
		{Object: lost.Key, NamedBy: "ENG/lost.md"},
		{Object: short.Key, NamedBy: "ENG/short.md", Damaged: true},
	}
	if r.Found == nil || r.Found.Missing != 1 || r.Found.Damaged != 2 ||
		!slices.Equal(r.Found.MissingFiles, want) {
		t.Fatalf("the findings = %+v, want %+v", r.Found, want)
	}
}

// A STORE THAT HAD NOT CAUGHT UP IS NOT ONE THAT LOST THE OBJECT: a "not found"
// is asked again at the end of the pass before the object is called missing.
func TestALaggingNotFoundIsNotMissing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
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
	h.backend.statErr = errors.New("the broker stopped answering")
	if _, err := h.c.Audit(t.Context()); err == nil {
		t.Fatal("an audit whose every question failed succeeded")
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
// where it is newer than its own — and a record from the build before, which
// kept no findings beside the attempt, gives its attempt's findings as found.
func TestRestoreKeepsTheNewerOfEachHalf(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	at := h.clock.Now()
	lost := MissingFile{Object: objstore.KeyAt(at), NamedBy: "ENG/lost.md"}
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

	// A RECORD THE BUILD BEFORE WROTE, with no findings of its own.
	old := newHarness(t)
	old.c.Restore(Status{Audit: AuditReport{Completed: true, Referenced: 4, Missing: 2, At: at}})
	if f := old.c.Status().Audit.Found; f == nil || f.Missing != 2 || f.Referenced != 4 || !f.At.Equal(at) {
		t.Fatalf("an older build's audit restored as findings %+v", f)
	}
}
