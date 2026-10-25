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

// rangesCounted is an S3 bucket that counts the ranges the audit asks it for,
// one a chunk, beside the requests the fake counts.
type rangesCounted struct {
	*s3obj.Backend
	ranges atomic.Int64
}

func (r *rangesCounted) ListRange(ctx context.Context, after, through string, visit func(objstore.Info) error) error {
	r.ranges.Add(1)
	return r.Backend.ListRange(ctx, after, through, visit)
}

// AND ON S3 THAT IS ONE LISTING REQUEST A CHUNK, whatever the bucket keeps
// between the references: a chunk is cut so its range fills the bucket's own
// page short of a room ([rangePageRoom]) at the density the ranges before it
// listed ([auditor.cut]), and the range ends on the page that reaches its last
// key — never a second request to learn that nothing follows. Cut at a
// constant chunk instead, a range holding more besides its references than
// the room needed a second request, which listed most of the next chunk's
// range besides: the bucket's everyday garbage, or a derived table's objects
// among the references — an index kept over packs at one to every one to
// eight of them — doubled the audit. Only the first range, cut before
// any was listed, may span the pages its names fill; and no range is cut
// shorter than the names it lists need.
func TestAnS3BucketIsAuditedAListingRequestAChunk(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// every is how many referenced objects are uploaded to each the
		// store holds that no required row names — zero for none.
		every int
	}{
		{"every object referenced", 0},
		{"one object in a hundred unnamed", 100},
		{"one object in twenty unnamed", 20},
		{"one derived object to every eight referenced", 8},
		{"one derived object to every four referenced", 4},
		{"a derived object beside every referenced one", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := s3fake.Start(t, "files")
			srv.PageSize = rangePage // what S3 answers a page, at most
			b, err := s3obj.Open(t.Context(), s3obj.Config{
				Endpoint: srv.URL, Region: "us-east-1", Bucket: "files", Prefix: "acme/",
				PathStyle: true, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
			})
			if err != nil {
				t.Fatal(err)
			}
			bucket := &rangesCounted{Backend: b}
			store, err := objstore.NewStore(bucket)
			if err != nil {
				t.Fatal(err)
			}
			src := &fakeSource{named: map[objstore.Key]objstore.Reference{}}
			const files = 3*rangeChunk + 300
			names := 0
			for i := range files {
				put := func(body string) objstore.Object {
					o, err := store.Put(t.Context(), strings.NewReader(body), 1<<20, objstore.PutMeta{})
					if err != nil {
						t.Fatal(err)
					}
					names++
					return o
				}
				o := put(fmt.Sprintf("object %d", i))
				src.named[o.Key] = objstore.Reference{Object: o, NamedBy: fmt.Sprintf("ENG/%04d.md", i)}
				if c.every > 0 && i%c.every == 0 {
					// ITS KEY BETWEEN TWO REFERENCED ONES, as a replaced
					// file's or a derived row's object's is: the audit
					// passes over either alike.
					put(fmt.Sprintf("not a live row's %d", i))
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
			// THE FIRST RANGE holds a whole chunk of references and every
			// object between them: the pages those fill.
			first := rangeChunk
			if c.every > 0 {
				first += (rangeChunk-2)/c.every + 1
			}
			firstPages := (first + rangePage - 1) / rangePage
			requests, ranges := srv.RequestsOf("ListObjectsV2"), int(bucket.ranges.Load())
			if requests > ranges-1+firstPages {
				t.Errorf("the audit listed %d ranges in %d requests; want one a range but the first's %d",
					ranges, requests, firstPages)
			}
			if most := (names+rangeChunk-1)/rangeChunk + 1; ranges > most {
				t.Errorf("the audit cut %d names into %d ranges, past the %d their pages need", names, ranges, most)
			}
			if c.every == 0 && (ranges != (files+rangeChunk-1)/rangeChunk || requests != ranges) {
				t.Errorf("a bucket holding nothing but references was listed in %d ranges and %d "+
					"requests, want a whole chunk a range and a request each", ranges, requests)
			}
			if n := srv.RequestsOf("HeadObject"); n != 0 {
				t.Errorf("the audit made %d HEAD requests, want none", n)
			}
		})
	}
}

// THE CUT FOLLOWS THE DENSITY OF THE RANGES LISTED: nothing measured, or a
// store keeping nothing between its references — or holding less than they
// name — cuts the whole chunk; the first range listed is taken as it is; a
// later one moves the estimate part of the way ([rangeDensityWeight]), so one
// range crowded by a burst of uploads does not halve the ranges after it,
// while a store that turns crowded for good is followed within some fifteen
// ranges; and however crowded, a range holds a reference.
func TestTheCutFollowsTheDensityOfTheRangesListed(t *testing.T) {
	t.Parallel()
	a := &auditor{}
	if n := a.cut(rangeChunk); n != rangeChunk {
		t.Fatalf("with nothing measured the cut is %d, want the chunk of %d", n, rangeChunk)
	}
	a.measured(0, rangeChunk) // a range whose every object the store lost
	a.measured(rangeChunk, rangeChunk)
	if n := a.cut(rangeChunk); n != rangeChunk {
		t.Fatalf("over a store holding only its references the cut is %d, want %d", n, rangeChunk)
	}
	a.measured(2*rangeChunk, rangeChunk)
	if n := a.cut(rangeChunk); n < rangeChunk*3/4 {
		t.Errorf("one crowded range after an uncrowded one cut the next to %d of %d", n, rangeChunk)
	}
	for range 15 {
		a.measured(2*rangeChunk, rangeChunk)
	}
	if n := a.cut(rangeChunk); n > rangeChunk*11/20 {
		t.Errorf("after sixteen ranges holding twice their references in names the cut is %d, "+
			"want near half the chunk of %d", n, rangeChunk)
	}

	first := &auditor{}
	first.measured(2*rangeChunk, rangeChunk)
	if n := first.cut(rangeChunk); n != rangeChunk/2 {
		t.Errorf("the first range's density is not taken as it is: the cut is %d, want %d", n, rangeChunk/2)
	}
	thin := &auditor{}
	thin.measured(rangeChunk/2, rangeChunk) // half the references lost or listed late
	if n := thin.cut(rangeChunk); n != rangeChunk {
		t.Errorf("a range listing fewer names than references cut the next to %d, want the chunk of %d",
			n, rangeChunk)
	}
	crowded := &auditor{}
	crowded.measured(1000*rangeChunk, rangeChunk)
	if n := crowded.cut(rangeChunk); n != 1 {
		t.Errorf("a range a thousand times crowded cut the next to %d references, want one", n)
	}
}

// THE WALK CUTS EACH RANGE FROM THE DENSITY THE RANGES BEFORE IT LISTED: over
// a store that keeps an object no required row names beside every reference,
// the first range — cut before any was listed — holds a whole chunk of
// references and so twice the chunk in names, and every later range half the
// references, its names inside the chunk, so each fits the page it is cut
// for.
func TestTheWalkCutsEachRangeFromTheDensityItListed(t *testing.T) {
	t.Parallel()
	h, r := newRangedHarness(t)
	h.c.rangeChunk = 10
	// A MILLISECOND APART, so every key sorts after the one minted before
	// it: the harness's clock stands still, and keys minted in one
	// millisecond sort at random.
	var refs []objstore.Key
	for i := range 100 {
		h.clock.advance(time.Millisecond)
		refs = append(refs, h.file(fmt.Sprintf("%03d.md", i), fmt.Sprintf("object %d", i)).Key)
		h.clock.advance(time.Millisecond)
		h.put(fmt.Sprintf("replaced %d", i))
	}
	var (
		mu     sync.Mutex
		ranges [][2]string
	)
	r.onList = func(_ context.Context, after, through string) error {
		mu.Lock()
		defer mu.Unlock()
		ranges = append(ranges, [2]string{after, through})
		return nil
	}
	got, err := h.c.Audit(t.Context())
	if err != nil || !got.Completed || got.Referenced != len(refs) || got.Found.Lost() != 0 {
		t.Fatalf("Audit = %+v, %v; want every file found whole", got, err)
	}
	var stored []string
	if err := h.backend.mem.List(t.Context(), func(info objstore.Info) error {
		stored = append(stored, info.Name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(stored)
	named := map[string]bool{}
	for _, k := range refs {
		named[k.Name()] = true
	}
	slices.SortFunc(ranges, func(a, b [2]string) int { return strings.Compare(a[1], b[1]) })
	for i, rg := range ranges {
		var held, references int
		for _, name := range stored {
			if name > rg[0] && name <= rg[1] {
				held++
				if named[name] {
					references++
				}
			}
		}
		switch {
		case i == 0 && references != h.c.rangeChunk:
			t.Errorf("the first range holds %d references, want the whole chunk of %d: no "+
				"range had been listed when it was cut", references, h.c.rangeChunk)
		case i > 0 && i < len(ranges)-1 && (held > h.c.rangeChunk || references < h.c.rangeChunk/2-1):
			t.Errorf("range %d holds %d names for %d references; want the chunk's %d names at most, "+
				"for about half as many references", i, held, references, h.c.rangeChunk)
		}
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

// A RANGE THE STORE CANNOT GIVE IN ORDER TURNS THE REST OF THE WALK TO THE
// STORE'S LISTING, as for a store that keeps no order — and the range already
// handed on is asked about by name — rather than reading every object the
// order lost as missing. The listing starts after the last range handed on,
// so no reference is looked at twice. And since the walk cuts no second range
// before its first has come back, one range is all a store that keeps no order
// is ever asked for: its first answer turns the walk before another goes out.
func TestARangeOutOfOrderTurnsTheAuditToTheListing(t *testing.T) {
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
	if n := h.backend.listings(); n != 1 {
		t.Errorf("the store was listed whole %d times after its ranges came back out of "+
			"order, want once", n)
	}
	h.backend.mu.Lock()
	defer h.backend.mu.Unlock()
	if len(h.backend.asks) != h.c.rangeChunk {
		t.Fatalf("%d of %d objects asked about by name; want those of the one range handed on "+
			"before the first answer, %d, and the rest read from the listing", len(h.backend.asks),
			files, h.c.rangeChunk)
	}
	for name, n := range h.backend.asks {
		if n != 1 {
			t.Errorf("%s was asked about %d times, want once", name, n)
		}
	}
	if n := r.listings(); n != 1 {
		t.Errorf("%d ranges were listed of a store that keeps no order; the first answer "+
			"should have stopped them before a second was cut", n)
	}
}

// A LISTING THAT FAILS SETS ITS RANGE ASIDE, and the audit runs on: the range
// takes its second look AS A RANGE once the walk is over — ONE listing more —
// and only an object that listing passes over is asked about by name. A
// listing fails once its deadline passes with the bucket still throttling or
// failing it — nothing counts its attempts; a HEAD for each object of the
// range, as there once was, sent the prefix that had just said to slow down
// a request for every object the listing would have answered in one.
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
	for i := range objstore.WalkPage + 10 {
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
// is a question like a stat, under the same closed loop. The first range is let
// through, since the walk cuts no second range before its first has come back.
func TestTheAuditListsAtMostItsBoundOfRangesAtOnce(t *testing.T) {
	t.Parallel()
	h, r := newRangedHarness(t)
	h.c.rangeChunk = 3
	for i := range 3 * 3 * auditConcurrency {
		h.file(fmt.Sprintf("%03d.md", i), fmt.Sprintf("object %d", i))
	}
	g := newGate()
	var first sync.Once
	r.onList = func(ctx context.Context, _, _ string) error {
		probe := false
		first.Do(func() { probe = true })
		if probe {
			return nil
		}
		return g.pass(ctx)
	}
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

func (s *twoTables) Required(statelog.Position) []objstore.Walk {
	s.mu.Lock()
	defer s.mu.Unlock()
	sorted := slices.SortedFunc(maps.Values(s.named), func(a, b objstore.Reference) int {
		return a.Object.Key.Compare(b.Object.Key)
	})
	var even, odd []objstore.Reference
	for i, ref := range sorted {
		if i%2 == 0 {
			even = append(even, ref)
		} else {
			odd = append(odd, ref)
		}
	}
	return []objstore.Walk{&heldWalk{pages: [][]objstore.Reference{even[:10], even[10:]}},
		&heldWalk{pages: [][]objstore.Reference{odd}}}
}

// heldWalk is a walk over pages held in memory.
type heldWalk struct{ pages [][]objstore.Reference }

func (w *heldWalk) Next(context.Context) ([]objstore.Reference, bool, error) {
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
	backwards := &heldSource{fakeSource: h.source, walk: &heldWalk{pages: [][]objstore.Reference{
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
	walk objstore.Walk
}

func (s *heldSource) Required(statelog.Position) []objstore.Walk { return []objstore.Walk{s.walk} }
