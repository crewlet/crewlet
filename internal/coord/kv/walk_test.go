package kv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// truncatingKV is a bucket whose listing STOPS WITHOUT SAYING SO.
//
// Embedding the interface rather than implementing it: only Watch and Bucket
// are reached, and a method this test does not mean to exercise should panic
// rather than quietly answer a zero value.
type truncatingKV struct {
	jetstream.KeyValue
	deliver int // entries handed over before the channel closes
}

func (k truncatingKV) Bucket() string { return "truncating" }

func (k truncatingKV) Watch(context.Context, string, ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	ch := make(chan jetstream.KeyValueEntry, k.deliver+1)
	for i := range k.deliver {
		ch <- stubEntry{key: fmt.Sprintf("k%d", i)}
	}
	// CLOSED, never the nil end-of-initial-values marker. This is what the
	// client's own watcher does when its subscription dies mid-listing.
	close(ch)
	return stubWatcher{ch: ch}, nil
}

type stubWatcher struct{ ch chan jetstream.KeyValueEntry }

func (w stubWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.ch }
func (w stubWatcher) Stop() error                             { return nil }

type stubEntry struct {
	jetstream.KeyValueEntry
	key string
}

func (e stubEntry) Key() string      { return e.key }
func (e stubEntry) Value() []byte    { return []byte("{}") }
func (e stubEntry) Revision() uint64 { return 1 }

// A LISTING THAT ENDS EARLY IS AN ERROR, NEVER A SHORT LIST.
//
// This is the whole reason this package does not use the client's ListKeys.
// That helper's goroutine ends on a nil entry, and a receive from the channel
// its subscription closes on failure yields exactly that nil — so a listing
// cut off half way came back TRUNCATED WITH A NIL ERROR. This package's rule
// is that "held", "definitively not held" and "the store could not be
// reached" are three different facts; a short list with no error collapses the
// third into the second at every caller at once, and for the trim's published
// floor that is a delete of records a node still needs.
func TestAListingThatEndsEarlyIsUnavailableRatherThanShort(t *testing.T) {
	t.Parallel()
	for _, delivered := range []int{0, 3} {
		t.Run(fmt.Sprintf("after_%d_entries", delivered), func(t *testing.T) {
			t.Parallel()
			var seen int
			err := watchWalk(context.Background(), truncatingKV{deliver: delivered}, jetstream.AllKeys, "the bucket",
				func(jetstream.KeyValueEntry) error { seen++; return nil })
			if err == nil {
				t.Fatalf("a listing that ended after %d of an unknown number of "+
					"entries returned no error; the caller reads that as the whole "+
					"bucket", seen)
			}
			if !errors.Is(err, coord.ErrUnavailable) {
				t.Errorf("error = %v, want it to carry coord.ErrUnavailable so the "+
					"caller can tell it from an empty bucket", err)
			}
			if seen != delivered {
				t.Errorf("visited %d entries, want the %d delivered before the cut", seen, delivered)
			}
		})
	}
}

// openFleetForTest opens a fleet store with retentions long enough that
// nothing lapses under a case that did not ask it to — the same reasoning
// TestFleetContract gives for its own numbers.
func openFleetForTest(t *testing.T, nc *nats.Conn, prefix string) *FleetStore {
	t.Helper()
	store, err := OpenFleet(context.Background(), nc, FleetConfig{
		BucketPrefix:    prefix,
		RateWindow:      time.Minute,
		ClaimTTL:        10 * time.Minute,
		LedgerRetention: 10 * time.Minute,
		FireRetention:   10 * time.Minute,
		FollowRetention: 10 * time.Minute,
		CooldownMax:     time.Hour,
		BudgetRetention: time.Hour,
		StatusFreshness: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("OpenFleet: %v", err)
	}
	return store
}

// AND AN EMPTY BUCKET IS NOT AN ERROR. The nil entry is the end of the initial
// values and the only thing that ends a walk successfully — so "there are no
// keys" has to stay distinguishable from the case above.
func TestAnEmptyBucketWalksCleanly(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
	store := openFleetForTest(t, nc, prefix)

	got, err := store.Usage(context.Background(), coord.WindowsAt(time.Now(), time.UTC))
	if err != nil {
		t.Fatalf("listing an empty bucket: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty bucket listed %d rows", len(got))
	}
}

// NO ENUMERATION LEAVES A CONSUMER BEHIND, INCLUDING ONE THAT ABANDONS THE
// WALK.
//
// The shape this replaced could not keep that promise. The client's key lister
// hands keys over a 256-buffered channel with a BLOCKING send, so a caller
// that returned early — which five of these methods do on a bad record — left
// the goroutine parked on that send for ever, and the server-side consumer
// with it. watchWalk owns the watcher and stops it on every exit, so the walk
// has no early-return path that leaks.
func TestAnAbandonedWalkLeavesNoConsumer(t *testing.T) {
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
	store := openFleetForTest(t, nc, prefix)
	ctx := context.Background()

	// Past the client lister's 256-entry buffer, so the shape this replaced
	// would park rather than merely linger.
	const keys = 300
	good, err := encodeTally(coord.Tally{At: time.Now().UTC()}.Roll(coord.WindowsAt(time.Now(), time.UTC)).Add(1, time.Now().UTC()))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := range keys {
		if _, err := store.budgets.Put(ctx, encodeKey(fmt.Sprintf("scope-%03d", i)), good); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	// The ORDERED walk specifically. This invariant belongs to the transport
	// that has a consumer at all — walking the bucket through eachEntry on a
	// broker that answers a batched read would assert nothing, since that
	// transport never creates one to leak.
	abandon := errors.New("the caller gave up on the first record")
	if err := watchWalk(ctx, store.budgets, jetstream.AllKeys, "the bucket", func(jetstream.KeyValueEntry) error {
		return abandon
	}); !errors.Is(err, abandon) {
		t.Fatalf("watchWalk = %v, want the visit's own error back unwrapped", err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	stream, err := js.Stream(ctx, "KV_"+prefix+budgetSuffix)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.State.Consumers != 0 {
		names := []string{}
		for name := range stream.ListConsumers(ctx).Info() {
			names = append(names, name.Name)
		}
		t.Errorf("the abandoned walk left %d consumer(s) on %s: %s",
			info.State.Consumers, stream.CachedInfo().Config.Name, strings.Join(names, ", "))
	}
}

// A FAILED LISTING NAMES THE LISTING, not the bucket it lives in.
//
// Seven key classes share the positions register, so every one of them used to
// fail with "read crewlet_positions" — a sentence that names the file an
// operator would inspect and never the duty that stalled. The trim floors, the
// trim holds and the maintenance acknowledgements are three very different
// outages behind that one message.
func TestAFailedListingNamesTheListingRatherThanTheBucket(t *testing.T) {
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("p%d", bucketSeq.Add(1))
	store := openFleetForTest(t, nc, prefix)

	// A cancelled walk is the cheapest failure both transports share, and
	// the message is composed in the same place every other failure's is.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	const listing = "the trim floors"
	err := store.eachUnder(ctx, store.positions, coord.DocumentFilter("floor"), listing,
		func(jetstream.KeyValueEntry) error { return nil })
	if err == nil {
		t.Fatal("a cancelled walk came back clean")
	}
	if !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), listing) {
		t.Errorf("err = %q, which does not name %q. Seven classes share this "+
			"bucket, so a message naming only the bucket is the same sentence "+
			"for all of them", err, listing)
	}
}
