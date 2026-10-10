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

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ranged is the harness's backend listing ranges in order, as an S3 bucket
// does — with what a bucket can do to a listing: leave an object out of it a
// while (hidden), and fail it (onList).
type ranged struct {
	*backend

	mu     sync.Mutex
	lists  int
	hidden map[string]bool
	onList func(ctx context.Context, after, through string) error
}

func (r *ranged) ListRange(ctx context.Context, after, through string, visit func(objstore.Info) error) error {
	r.mu.Lock()
	r.lists++
	hook, hidden := r.onList, maps.Clone(r.hidden)
	r.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, after, through); err != nil {
			return err
		}
	}
	return r.mem.ListRange(ctx, after, through, func(info objstore.Info) error {
		if hidden[info.Name] {
			return nil
		}
		return visit(info)
	})
}

func (r *ranged) listings() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lists
}

// stats is how many stats the backend has answered.
func (b *backend) stats() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, asked := range b.asks {
		n += asked
	}
	return n
}

// newRangedHarness is [newHarness] over a store that lists ranges in order.
func newRangedHarness(t *testing.T) (*harness, *ranged) {
	t.Helper()
	h := newHarness(t)
	r := &ranged{backend: h.backend, hidden: map[string]bool{}}
	store, err := objstore.NewStoreAt(r, h.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Store: store, References: References{h.source}, Now: h.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	h.store, h.c = store, c
	return h, r
}

// A STORE THAT LISTS IN ORDER IS AUDITED A RANGE AT A TIME: a listing of the
// range a chunk of references spans answers what a stat of each would, so the
// audit of a large store on S3 costs a request per chunk of objects rather
// than one per object.
func TestARangedStoreIsAuditedARangeToARequest(t *testing.T) {
	t.Parallel()
	h, r := newRangedHarness(t)
	const files = 2*rangeChunk + rangeChunk/2
	for i := range files {
		h.file(fmt.Sprintf("%04d.md", i), fmt.Sprintf("object %d", i))
	}
	// AND THE STORE'S OTHER OBJECTS between them — garbage a day leaves —
	// are listed past and cost nothing more than the bytes of their names.
	for i := range 40 {
		h.put(fmt.Sprintf("unnamed %d", i))
	}
	got, err := h.c.Audit(t.Context())
	if err != nil || !got.Completed || got.Referenced != files || got.Found.Lost() != 0 {
		t.Fatalf("Audit = %+v, %v; want every file found whole", got, err)
	}
	if n := r.listings(); n != 3 {
		t.Errorf("the audit made %d listings of %d files, want one a %d", n, files, rangeChunk)
	}
	if n := h.backend.stats(); n != 0 {
		t.Errorf("the audit asked about %d objects one at a time, want none", n)
	}
}

// AND ON S3 THAT IS ONE LISTING REQUEST A CHUNK, with the objects no required
// row names between the references in it: a chunk is cut short of the bucket's
// own page by the room those take ([rangePageRoom]), and the range ends on the
// page that reaches its last key — never a second request to learn that
// nothing follows. Cut at the page itself, a range holding one object besides
// its references needed a second request, which listed most of the next
// chunk's range besides — so the bucket's everyday garbage doubled the audit.
func TestAnS3BucketIsAuditedAListingRequestAChunk(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// every is how many referenced objects are uploaded to each the
		// store holds that no row names — zero for none.
		every int
	}{
		{"every object referenced", 0},
		{"one object in a hundred unnamed", 100},
		{"one object in twenty unnamed", 20},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := s3fake.Start(t, "files")
			srv.PageSize = 1000 // what S3 answers a page, at most
			b, err := s3obj.Open(t.Context(), s3obj.Config{
				Endpoint: srv.URL, Region: "us-east-1", Bucket: "files", Prefix: "acme/",
				PathStyle: true, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
			})
			if err != nil {
				t.Fatal(err)
			}
			store, err := objstore.NewStore(b)
			if err != nil {
				t.Fatal(err)
			}
			src := &fakeSource{named: map[objstore.Key]Reference{}}
			const files = 3*rangeChunk + 300
			for i := range files {
				put := func(body string) objstore.Object {
					o, err := store.Put(t.Context(), strings.NewReader(body), 1<<20, objstore.PutMeta{})
					if err != nil {
						t.Fatal(err)
					}
					return o
				}
				o := put(fmt.Sprintf("object %d", i))
				src.named[o.Key] = Reference{Object: o, NamedBy: fmt.Sprintf("ENG/%04d.md", i)}
				if c.every > 0 && i%c.every == 0 {
					put(fmt.Sprintf("replaced %d", i)) // its key between two referenced ones
				}
			}
			coll, err := New(Options{Store: store, References: References{src}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := coll.Audit(t.Context())
			if err != nil || !got.Completed || got.Referenced != files || got.Found.Lost() != 0 {
				t.Fatalf("Audit = %+v, %v; want every file found whole", got, err)
			}
			chunks := (files + rangeChunk - 1) / rangeChunk
			if n := srv.RequestsOf("ListObjectsV2"); n != chunks {
				t.Errorf("the audit of %d objects in %d chunks made %d listing requests, want one a chunk",
					files, chunks, n)
			}
			if n := srv.RequestsOf("HeadObject"); n != 0 {
				t.Errorf("the audit made %d HEAD requests, want none", n)
			}
		})
	}
}

// A RANGED AUDIT FINDS WHAT A STAT OF EACH WOULD: an object the listing passes
// over is asked about once more, by name, before it is called missing — a
// bucket that lists a new object late is not one that lost it — and one it
// lists at another size is damaged on that answer.
func TestARangedAuditFindsWhatAStatWould(t *testing.T) {
	t.Parallel()
	h, r := newRangedHarness(t)
	h.c.rangeChunk = 7
	var lost, late, short objstore.Object
	for i := range 40 {
		o := h.file(fmt.Sprintf("%02d.md", i), fmt.Sprintf("object %d", i))
		switch i {
		case 5:
			lost = o
		case 18:
			late = o
		case 29:
			short = o
		}
	}
	if err := h.store.Delete(t.Context(), lost.Key); err != nil {
		t.Fatal(err)
	}
	r.hidden[late.Key.Name()] = true
	h.backend.Corrupt(short.Key.Name(), []byte("rot"))

	got, err := h.c.Audit(t.Context())
	if err != nil || !got.Completed || got.Missing != 1 || got.Damaged != 1 || got.Unanswered != 0 {
		t.Fatalf("Audit = %+v, %v; want one missing and one damaged", got, err)
	}
	want := []MissingFile{
		{Object: lost.Key, NamedBy: "ENG/05.md", Finding: FindingMissing},
		{Object: short.Key, NamedBy: "ENG/29.md", Finding: FindingDamaged},
	}
	if !slices.Equal(got.Found.MissingFiles, want) {
		t.Fatalf("the findings = %+v, want %+v", got.Found.MissingFiles, want)
	}
	h.backend.mu.Lock()
	defer h.backend.mu.Unlock()
	if len(h.backend.asks) != 2 || h.backend.asks[lost.Key.Name()] != 1 || h.backend.asks[late.Key.Name()] != 1 {
		t.Fatalf("asked about by name: %v; want the two objects the listings passed over, once each", h.backend.asks)
	}
}

// A RANGE THE STORE CANNOT GIVE IN ORDER TURNS THE AUDIT TO ASKING BY NAME —
// that range's references included — rather than reading every object the
// order lost as missing.
func TestARangeOutOfOrderTurnsTheAuditToAskingByName(t *testing.T) {
	t.Parallel()
	h, r := newRangedHarness(t)
	h.c.rangeChunk = 10
	r.onList = func(context.Context, string, string) error {
		return fmt.Errorf("%w: a directory bucket", objstore.ErrUnordered)
	}
	// MORE RANGES THAN A RUN OF FAILURES: taken for a failed listing each,
	// they would stop the audit as a failing store.
	const files = 10*(failedInARow+8) + 5
	for i := range files {
		h.file(fmt.Sprintf("%03d.md", i), fmt.Sprintf("object %d", i))
	}
	got, err := h.c.Audit(t.Context())
	if err != nil || !got.Completed || got.Referenced != files || got.Found.Lost() != 0 {
		t.Fatalf("Audit = %+v, %v; want every file found whole", got, err)
	}
	h.backend.mu.Lock()
	defer h.backend.mu.Unlock()
	if len(h.backend.asks) != files {
		t.Fatalf("%d objects asked about by name, want all %d", len(h.backend.asks), files)
	}
	for name, n := range h.backend.asks {
		if n != 1 {
			t.Errorf("%s was asked about %d times, want once", name, n)
		}
	}
	if n := r.listings(); n > auditConcurrency {
		t.Errorf("%d listings were asked of a store that keeps no order; the first answer "+
			"should have stopped them", n)
	}
}

// A LISTING THAT FAILS SETS ITS RANGE ASIDE, and the audit runs on: the range
// takes its second look AS A RANGE once the walk is over — ONE listing more —
// and only an object that listing passes over is asked about by name. A
// listing fails once the SDK's retries are spent, which is what a bucket
// throttling the prefix does; a HEAD for each object of the range, as there
// once was, sent the prefix that had just said to slow down a request for
// every object the listing would have answered in one.
func TestAFailedListingSetsItsRangeAside(t *testing.T) {
	t.Parallel()
	h, r := newRangedHarness(t)
	h.c.rangeChunk = 10
	var failedOnce sync.Once
	r.onList = func(_ context.Context, after, _ string) error {
		var err error
		if after == "files/" {
			failedOnce.Do(func() { err = errors.New("SlowDown: please reduce your request rate") })
		}
		return err
	}
	var keys []objstore.Key
	for i := range 35 {
		keys = append(keys, h.file(fmt.Sprintf("%02d.md", i), fmt.Sprintf("object %d", i)).Key)
	}
	slices.SortFunc(keys, objstore.Key.Compare)
	// ONE OBJECT OF THE FAILED RANGE THE BUCKET LISTS LATE, which the
	// re-listing passes over: that one, and only that one, is asked about
	// by name before it is called anything.
	late := keys[4]
	r.hidden[late.Name()] = true
	got, err := h.c.Audit(t.Context())
	if err != nil || !got.Completed || got.Referenced != 35 || got.Found.Lost() != 0 {
		t.Fatalf("Audit = %+v, %v; want every file found whole", got, err)
	}
	if n := r.listings(); n != 4+1 {
		t.Errorf("the audit made %d listings of 4 ranges, one of which failed; want one more for that range", n)
	}
	h.backend.mu.Lock()
	defer h.backend.mu.Unlock()
	if len(h.backend.asks) != 1 || h.backend.asks[late.Name()] != 1 {
		t.Fatalf("asked about by name: %v; want only the object the re-listing passed over, once", h.backend.asks)
	}
}

// A RANGE SET ASIDE COUNTS AGAINST THE SECOND LOOK'S BOUND, every reference it
// holds, and is re-listed before the walk ends once the queue passes it — so a
// bucket that fails listing after listing never has the audit hold every
// reference those ranges span until the end of the walk.
//
// The last range's listing waits for the re-listing to have been made, so an
// audit that saved every range set aside for the end of its walk never
// finishes it.
func TestARangeSetAsideIsRelistedOnceTheQueueHoldsItsBound(t *testing.T) {
	t.Parallel()
	h, r := newRangedHarness(t)
	h.c.rangeChunk = 5
	h.c.suspectsHeld = 5
	var keys []objstore.Key
	for i := range 30 {
		keys = append(keys, h.file(fmt.Sprintf("%02d.md", i), fmt.Sprintf("object %d", i)).Key)
	}
	slices.SortFunc(keys, objstore.Key.Compare)
	// THE FIRST TWO RANGES' LISTINGS FAIL ONCE EACH: the two ranges set aside
	// hold twice the bound, so whichever is pushed second hands the other
	// back to be re-listed at once, in its own question's slot — whichever
	// order the two failures land in.
	var (
		mu       sync.Mutex
		asked    = map[string]int{}
		relisted = make(chan struct{})
		seen     sync.Once
	)
	failing := map[string]bool{"files/": true, keys[4].Name(): true}
	r.onList = func(_ context.Context, after, through string) error {
		mu.Lock()
		asked[after]++
		n := asked[after]
		mu.Unlock()
		switch {
		case failing[after] && n == 1:
			return errors.New("SlowDown: please reduce your request rate")
		case failing[after]:
			seen.Do(func() { close(relisted) })
		case through == keys[29].Name():
			select {
			case <-relisted:
			case <-time.After(10 * time.Second):
				t.Error("the last range was listed with both failed ones still set aside: every " +
					"range set aside was held for the end of the walk")
			}
		}
		return nil
	}
	got, err := h.c.Audit(t.Context())
	if err != nil || !got.Completed || got.Referenced != 30 || got.Found.Lost() != 0 {
		t.Fatalf("Audit = %+v, %v; want every file found whole", got, err)
	}
	if n := r.listings(); n != 6+2 {
		t.Errorf("the audit made %d listings of 6 ranges, two of which failed once; want 8", n)
	}
	if n := h.backend.stats(); n != 0 {
		t.Fatalf("the audit asked about %d objects by name, want none", n)
	}
}

// A RANGE WHOSE RE-LISTING FAILS TOO IS UNANSWERED, as an object whose two
// stats failed is: once the store answers a question after it — the audit's
// last, about an object it had answered for — every reference of the range is
// one the store would not answer about on either look, and named as such.
// None of them is asked about by name: the store failed both listings of the
// range, and a HEAD of each would send it a request for every object at the
// moment it is failing.
func TestARangeWhoseRelistingFailsIsUnanswered(t *testing.T) {
	t.Parallel()
	h, r := newRangedHarness(t)
	h.c.rangeChunk = 10
	r.onList = func(_ context.Context, after, _ string) error {
		if after == "files/" {
			return errors.New("SlowDown: please reduce your request rate")
		}
		return nil
	}
	for i := range 35 {
		h.file(fmt.Sprintf("%02d.md", i), fmt.Sprintf("object %d", i))
	}
	got, err := h.c.Audit(t.Context())
	if err != nil || !got.Completed || got.Referenced != 35 || got.Unanswered != 10 ||
		got.Missing != 0 || got.Damaged != 0 {
		t.Fatalf("Audit = %+v, %v; want the 10 files of the range listed in vain twice unanswered", got, err)
	}
	if n := r.listings(); n != 4+1 {
		t.Errorf("the audit made %d listings of 4 ranges; want one more for the range whose listing failed", n)
	}
	for _, f := range got.Found.MissingFiles {
		if f.Finding != FindingUnanswered {
			t.Errorf("%s was found %s, want unanswered", f.NamedBy, f.Finding)
		}
	}
	if n := h.backend.stats(); n != 1 {
		t.Fatalf("the audit asked about %d objects by name; want only the one question that "+
			"confirms the store was answering", n)
	}
}

// A RANGED AUDIT STOPPED MID-WALK HAS NOT RUN TO ITS END. A ranged walk
// gathers a whole chunk before it asks anything, so while it reads its pages
// no question is out, and a walk its context stops starts none that could
// fail: every question it did ask answered. What it counted is a floor, and
// reported as the findings it would replace the last whole audit's.
func TestARangedAuditStoppedMidWalkHasNotRunToItsEnd(t *testing.T) {
	t.Parallel()
	h, _ := newRangedHarness(t)
	// Two pages of references and one chunk of them, at the default chunk.
	for i := range eachPage + 10 {
		h.file(fmt.Sprintf("%04d.md", i), fmt.Sprintf("object %d", i))
	}
	ctx, stop := context.WithCancelCause(t.Context())
	defer stop(nil)
	why := errors.New("the duty moved")
	pages := 0
	h.source.pageRead = func() {
		pages++
		if pages == 2 {
			stop(why)
		}
	}
	r, err := h.c.Audit(ctx)
	if !errors.Is(err, why) {
		t.Fatalf("a ranged audit stopped mid-walk returned %v, want it to say it was "+
			"stopped by %v", err, why)
	}
	if r.Completed || r.Found != nil {
		t.Fatalf("a ranged audit stopped mid-walk reported %+v; want it incomplete, "+
			"with no findings of its own", r)
	}
}

// THE AUDIT LISTS UP TO ITS BOUND OF RANGES AT ONCE, AND NEVER MORE: a listing
// is a question like a stat, under the same closed loop.
func TestTheAuditListsAtMostItsBoundOfRangesAtOnce(t *testing.T) {
	t.Parallel()
	h, r := newRangedHarness(t)
	h.c.rangeChunk = 3
	for i := range 3 * 3 * auditConcurrency {
		h.file(fmt.Sprintf("%03d.md", i), fmt.Sprintf("object %d", i))
	}
	g := newGate()
	r.onList = func(ctx context.Context, _, _ string) error { return g.pass(ctx) }
	done := make(chan error, 1)
	go func() {
		_, err := h.c.Audit(t.Context())
		done <- err
	}()
	g.waitHeld(t, auditConcurrency)
	time.Sleep(50 * time.Millisecond)
	if _, peak := g.counts(); peak != auditConcurrency {
		t.Fatalf("the audit had %d listings out at once, past its bound of %d", peak, auditConcurrency)
	}
	close(g.open)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if held, peak := g.counts(); held != 0 || peak != auditConcurrency {
		t.Fatalf("after the audit the gate holds %d and held at most %d", held, peak)
	}
}

// EVERY REQUIRED TABLE IS MERGED INTO ONE KEY ORDER before it is cut into
// ranges: the ranges tile the key space, so a reference of one table that
// fell inside a range cut from another's keys would be one the listing never
// reached — asked about again by name for nothing. Merged, nothing is.
func TestTheAuditMergesEveryRequiredTableIntoOneKeyOrder(t *testing.T) {
	t.Parallel()
	h, r := newRangedHarness(t)
	h.c.rangeChunk = 4
	two := &twoTables{fakeSource: h.source}
	c, err := New(Options{Store: h.store, References: References{two}, Now: h.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	c.rangeChunk = 4
	for i := range 50 {
		h.file(fmt.Sprintf("%02d.md", i), fmt.Sprintf("object %d", i))
	}
	got, err := c.Audit(t.Context())
	if err != nil || !got.Completed || got.Referenced != 50 || got.Found.Lost() != 0 {
		t.Fatalf("Audit = %+v, %v; want every file of both tables found whole", got, err)
	}
	if n := h.backend.stats(); n != 0 {
		t.Fatalf("%d objects were asked about by name: a range cut from one table's keys "+
			"left out another's", n)
	}
	if n := r.listings(); n != 13 {
		t.Errorf("%d listings of 50 references four to a range, want 13", n)
	}
}

// twoTables is a source whose references are in two required tables, the
// keys of each interleaved with the other's.
type twoTables struct{ *fakeSource }

func (s *twoTables) Required(statelog.Position) []Walk {
	s.mu.Lock()
	defer s.mu.Unlock()
	sorted := slices.SortedFunc(maps.Values(s.named), func(a, b Reference) int {
		return a.Object.Key.Compare(b.Object.Key)
	})
	var even, odd []Reference
	for i, ref := range sorted {
		if i%2 == 0 {
			even = append(even, ref)
		} else {
			odd = append(odd, ref)
		}
	}
	return []Walk{&heldWalk{pages: [][]Reference{even[:10], even[10:]}},
		&heldWalk{pages: [][]Reference{odd}}}
}

// heldWalk is a walk over pages held in memory.
type heldWalk struct{ pages [][]Reference }

func (w *heldWalk) Next(context.Context) ([]Reference, bool, error) {
	if len(w.pages) == 0 {
		return nil, true, nil
	}
	page := w.pages[0]
	w.pages = w.pages[1:]
	return page, true, nil
}

// A WALK OUT OF KEY ORDER FAILS THE AUDIT rather than being cut into ranges
// that cannot hold what it hands on.
func TestAWalkOutOfKeyOrderFailsTheAudit(t *testing.T) {
	t.Parallel()
	h, _ := newRangedHarness(t)
	a := h.file("a.md", "a").Key
	b := h.file("b.md", "b").Key
	if a.Compare(b) > 0 {
		a, b = b, a
	}
	backwards := &heldSource{fakeSource: h.source, walk: &heldWalk{pages: [][]Reference{
		{h.source.named[b], h.source.named[a]},
	}}}
	c, err := New(Options{Store: h.store, References: References{backwards}, Now: h.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.Audit(t.Context()); err == nil || got.Completed {
		t.Fatalf("Audit over a walk out of key order = %+v, %v; want it refused", got, err)
	}
}

// heldSource is a fake source whose one required walk is given.
type heldSource struct {
	*fakeSource
	walk Walk
}

func (s *heldSource) Required(statelog.Position) []Walk { return []Walk{s.walk} }
