package collect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// batching is the harness's backend deleting in batches, as an S3 bucket does:
// each batch is recorded, then handed to onBatch, which may answer it in place
// of the twin.
type batching struct {
	*backend

	bmu     sync.Mutex
	batches [][]string
	onBatch func(ctx context.Context, names []string) error
}

func (b *batching) DeleteBatch(ctx context.Context, names []string) error {
	b.bmu.Lock()
	b.batches = append(b.batches, slices.Clone(names))
	hook := b.onBatch
	b.bmu.Unlock()
	if hook != nil {
		return hook(ctx, names)
	}
	return b.mem.DeleteBatch(ctx, names)
}

func (b *batching) sent() [][]string {
	b.bmu.Lock()
	defer b.bmu.Unlock()
	return slices.Clone(b.batches)
}

// newBatchingHarness is [newHarness] over a store that deletes in batches,
// counting the single deletions it is asked for besides.
func newBatchingHarness(t *testing.T) (*harness, *batching, *atomic.Int64) {
	t.Helper()
	h := newHarness(t)
	b := &batching{backend: h.backend}
	store, err := objstore.NewStoreAt(b, h.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Store: store, References: References{h.source}, Now: h.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	var singles atomic.Int64
	h.backend.onDelete = func(context.Context, string) error {
		singles.Add(1)
		return nil
	}
	h.store, h.c = store, c
	return h, b, &singles
}

// ON A STORE THAT DELETES IN BATCHES A COLLECTION DELETES A BATCH AT A TIME:
// each judged batch's unnamed objects go in one request — a bulk removal on S3
// is a request per five hundred objects rather than one per object — with
// [batchesInFlight] of them out at once and never more, and none deleted one by
// one. Every object deleted is counted, and none that is named is touched.
func TestACollectionDeletesABatchAtATimeOnAStoreThatDeletesInBatches(t *testing.T) {
	t.Parallel()
	h, b, singles := newBatchingHarness(t)
	var orphans, kept []objstore.Key
	for i := range 2*JudgeBatch + 7 {
		orphans = append(orphans, h.put(fmt.Sprintf("orphan %d", i)))
		if i%100 == 0 {
			kept = append(kept, h.file(fmt.Sprintf("%03d.md", i), fmt.Sprintf("kept %d", i)).Key)
		}
	}
	h.clock.advance(PendingGrace + time.Hour)
	g := newGate()
	b.onBatch = func(ctx context.Context, names []string) error {
		if err := g.pass(ctx); err != nil {
			return err
		}
		return b.mem.DeleteBatch(ctx, names)
	}
	type result struct {
		r   CollectionReport
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := h.c.Collect(t.Context())
		done <- result{r, err}
	}()
	g.waitHeld(t, batchesInFlight)
	time.Sleep(50 * time.Millisecond)
	if _, peak := g.counts(); peak != batchesInFlight {
		t.Fatalf("the collection had %d batches in flight, past its bound of %d", peak, batchesInFlight)
	}
	close(g.open)
	got := <-done
	if got.err != nil || !got.r.Completed || got.r.Deleted != len(orphans) || got.r.Referenced != len(kept) {
		t.Fatalf("Collect = %+v, %v; want all %d orphans deleted and %d named kept",
			got.r, got.err, len(orphans), len(kept))
	}
	listed := got.r.Aged
	if want := (listed + JudgeBatch - 1) / JudgeBatch; len(b.sent()) != want {
		t.Fatalf("the collection sent %d batches for %d judged objects, want one a judged batch (%d)",
			len(b.sent()), listed, want)
	}
	if n := singles.Load(); n != 0 {
		t.Fatalf("the collection deleted %d objects one by one on a store that deletes in batches", n)
	}
	for _, k := range orphans {
		if h.held(k) {
			t.Fatalf("%s survived a collection that reported it deleted", k)
		}
	}
	for _, k := range kept {
		if !h.held(k) {
			t.Fatalf("%s, which a row names, was deleted", k)
		}
	}
}

// A BATCH THE STORE REFUSES AS A REQUEST TURNS THE COLLECTION TO DELETING EACH
// OBJECT ON ITS OWN — an S3-compatible gateway without the verb — and the pass
// still deletes everything it judged, asking that store for a batch once:
// failing every collection on such a gateway would keep its garbage, and
// billed, for ever.
func TestABatchRefusedAsARequestTurnsTheCollectionToSingleDeletions(t *testing.T) {
	t.Parallel()
	h, b, singles := newBatchingHarness(t)
	var orphans []objstore.Key
	for i := range 2*JudgeBatch + 7 {
		orphans = append(orphans, h.put(fmt.Sprintf("orphan %d", i)))
	}
	h.clock.advance(PendingGrace + time.Hour)
	b.onBatch = func(context.Context, []string) error {
		return fmt.Errorf("%w: NotImplemented", objstore.ErrNoBatch)
	}
	r, err := h.c.Collect(t.Context())
	if err != nil || !r.Completed || r.Deleted != len(orphans) {
		t.Fatalf("Collect = %+v, %v; want all %d orphans deleted one by one", r, err, len(orphans))
	}
	if n := len(b.sent()); n != 1 {
		t.Fatalf("a store that refused the verb was asked for a batch %d times, want once", n)
	}
	if n := singles.Load(); n != int64(len(orphans)) {
		t.Fatalf("%d single deletions for %d orphans", n, len(orphans))
	}
	for _, k := range orphans {
		if h.held(k) {
			t.Fatalf("%s survived a collection that reported it deleted", k)
		}
	}
}

// A BATCH THE STORE ANSWERED AND REFUSED IN PART STOPS THE COLLECTION, as a
// failed deletion does, and the report counts exactly what it removed: a
// deletion the store refused is the store failing at the one thing the pass
// asks of it, and the next pass tries again.
func TestABatchRefusedInPartStopsTheCollection(t *testing.T) {
	t.Parallel()
	h, b, _ := newBatchingHarness(t)
	var orphans []objstore.Key
	for i := range 2 * JudgeBatch {
		orphans = append(orphans, h.put(fmt.Sprintf("orphan %d", i)))
	}
	h.clock.advance(PendingGrace + time.Hour)
	var first sync.Once
	b.onBatch = func(ctx context.Context, names []string) error {
		var err error
		first.Do(func() {
			if derr := b.mem.DeleteBatch(ctx, names[1:]); derr != nil {
				err = derr
				return
			}
			err = &objstore.DeleteBatchError{Failed: map[string]error{names[0]: errors.New("AccessDenied")}}
		})
		if err != nil {
			return err
		}
		return b.mem.DeleteBatch(ctx, names)
	}
	r, err := h.c.Collect(t.Context())
	var refused *objstore.DeleteBatchError
	if !errors.As(err, &refused) || r.Completed {
		t.Fatalf("Collect = %+v, %v; want it failed with the batch's refusal", r, err)
	}
	gone := 0
	for _, k := range orphans {
		if !h.held(k) {
			gone++
		}
	}
	if r.Deleted != gone || gone != JudgeBatch-1 {
		t.Fatalf("the collection reported %d deleted and the store lost %d; want both the %d "+
			"the refused batch removed, and nothing after it", r.Deleted, gone, JudgeBatch-1)
	}
}
