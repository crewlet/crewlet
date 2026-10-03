package kv

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

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
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

// A LISTING THAT ENDS EARLY IS AN ERROR, NEVER A SHORT LIST — and it hands
// the caller NOTHING, rather than the part it read before the cut.
//
// This is the whole reason this package does not use the client's ListKeys.
// That helper's goroutine ends on a nil entry, and a receive from the channel
// its subscription closes on failure yields exactly that nil — so a listing
// cut off half way came back TRUNCATED WITH A NIL ERROR. This package's rule
// is that "held", "definitively not held" and "the store could not be
// reached" are three different facts; a short list with no error collapses the
// third into the second at every caller at once, and for the trim's published
// floor that is a delete of records a node still needs.
//
// The visit count is the second half. A walk that streamed into its caller
// handed over the entries delivered before the cut and THEN failed, so a
// caller accumulating as it went held a partial listing next to the error.
func TestAListingThatEndsEarlyIsUnavailableRatherThanShort(t *testing.T) {
	t.Parallel()
	js := jsOf(embeddedNATS(t))
	for _, delivered := range []int{0, 3} {
		t.Run(fmt.Sprintf("after_%d_entries", delivered), func(t *testing.T) {
			t.Parallel()
			var seen int
			err := eachEntryUnder(context.Background(), js, truncatingKV{deliver: delivered},
				jetstream.AllKeys, "the bucket",
				func(jetstream.KeyValueEntry) error { seen++; return nil })
			if err == nil {
				t.Fatalf("a listing that ended after %d of an unknown number of "+
					"entries returned no error; the caller reads that as the whole "+
					"bucket", delivered)
			}
			if !errors.Is(err, coord.ErrUnavailable) {
				t.Errorf("error = %v, want it to carry coord.ErrUnavailable so the "+
					"caller can tell it from an empty bucket", err)
			}
			if !strings.Contains(err.Error(), "listing ended early") {
				t.Errorf("error = %v, want the PASS's failure: the cut is what "+
					"happened, and the key index this bucket does not have is not", err)
			}
			if seen != 0 {
				t.Errorf("visited %d entries of a listing that failed; a failed "+
					"listing hands its caller nothing", seen)
			}
		})
	}
}

// openFleetForTest opens a fleet store with retentions long enough that
// nothing lapses under a case that did not ask it to — the same reasoning
// TestFleetContract gives for its own numbers.
func openFleetForTest(t *testing.T, nc *nats.Conn, prefix string) *FleetStore {
	t.Helper()
	return openFleetVia(t, jsOf(nc), prefix)
}

// openFleetVia is [openFleetForTest] over a client in any API — a leaf's,
// which addresses the embedded fleet's domain.
//
// The retentions are the suite's, not production's: every case reasons about
// a window or a claim it names both sides of, and a bucket sized for the
// production ledger's seven days would make a "this has lapsed" case wait
// seven days. Every one is above the broker's 100 ms floor and far longer
// than a case takes, so nothing lapses under a case that did not ask it to.
func openFleetVia(t *testing.T, client jetstream.JetStream, prefix string) *FleetStore {
	t.Helper()
	store, err := OpenFleet(context.Background(), client, FleetConfig{
		BucketPrefix:    prefix,
		RateWindow:      time.Minute,
		ClaimTTL:        10 * time.Minute,
		LedgerRetention: 10 * time.Minute,
		FireRetention:   10 * time.Minute,
		FollowRetention: 10 * time.Minute,
		RebaseRetention: 10 * time.Minute,
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

	abandon := errors.New("the caller gave up on the first record")
	if err := store.each(ctx, store.budgets, func(jetstream.KeyValueEntry) error {
		return abandon
	}); !errors.Is(err, abandon) {
		t.Fatalf("each = %v, want the visit's own error back unwrapped", err)
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
// Eight key classes share the positions register, so every one of them used to
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
		t.Errorf("err = %q, which does not name %q. Eight classes share this "+
			"bucket, so a message naming only the bucket is the same sentence "+
			"for all of them", err, listing)
	}
}

// probeKV is a REAL bucket whose pass LOSES one key.
//
// That is how the broker's own failure is reproduced deterministically. What
// the pass does on a busy bucket — end its initial values while a key's only
// revision is still ahead of the cursor — is a race against the broker's
// signal queue; what it LEAVES is a key the pass never delivered while the
// stream still holds it, and that is exactly what this hands the listing
// every time. Everything else — the index read, the certifying read, the
// stream — is the broker's own.
type probeKV struct {
	jetstream.KeyValue
	lose string // a key whose revisions the pass never delivers
}

func (k probeKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	inner, err := k.KeyValue.Watch(ctx, keys, opts...)
	if err != nil || k.lose == "" {
		return inner, err
	}
	w := &losingWatcher{
		inner: inner,
		out:   make(chan jetstream.KeyValueEntry),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go func() {
		defer close(w.done)
		defer close(w.out)
		for {
			select {
			case <-w.stop:
				return
			case kve, ok := <-inner.Updates():
				if !ok {
					return
				}
				if kve != nil && kve.Key() == k.lose {
					continue
				}
				select {
				case w.out <- kve:
				case <-w.stop:
					return
				}
			}
		}
	}()
	return w, nil
}

// losingWatcher forwards a real watcher's entries, bar the lost key's.
type losingWatcher struct {
	inner jetstream.KeyWatcher
	out   chan jetstream.KeyValueEntry
	stop  chan struct{}
	once  sync.Once
	done  chan struct{}
}

func (w *losingWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.out }

func (w *losingWatcher) Stop() error {
	w.once.Do(func() { close(w.stop) })
	err := w.inner.Stop()
	<-w.done
	return err
}

// putBudget writes one scope's counter the way the store does.
func putBudget(ctx context.Context, t *testing.T, kv jetstream.KeyValue, scope string, used int) {
	t.Helper()
	var tally coord.Tally
	tally.Slots[0] = coord.Slot{Label: "test", Used: used}
	tally.At = time.Now().UTC()
	raw, err := encodeTally(tally)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := kv.Put(ctx, encodeKey(scope), raw); err != nil {
		t.Fatalf("put %s: %v", scope, err)
	}
}

// wireReads counts, ON THE WIRE, the single-key reads a listing sends one
// bucket's stream: the leader's `STREAM.MSG.GET` and the direct gets any
// replica may answer.
//
// The wire rather than a wrapper, because the question is what the broker was
// ASKED — a wrapper around a read function counts calls to that function and
// is blind to a second path that reaches the broker another way, which is
// exactly the shape of the defect these counts guard: a certification that
// looked like a read and went out as a direct get.
type wireReads struct {
	conn   *nats.Conn
	leader *nats.Subscription
	direct []*nats.Subscription
}

// countWireReads starts counting on nc, which must be the connection the
// listing's own client rides: a subscriber sees each request before the
// broker's answer to it reaches the same connection, so a count taken after
// the listing returns has seen every request it made.
func countWireReads(t *testing.T, nc *nats.Conn, kv jetstream.KeyValue) *wireReads {
	t.Helper()
	sub := func(subject string) *nats.Subscription {
		s, err := nc.SubscribeSync(subject)
		if err != nil {
			t.Fatalf("subscribe %s: %v", subject, err)
		}
		t.Cleanup(func() { _ = s.Unsubscribe() })
		return s
	}
	stream := bucketStream(kv)
	w := &wireReads{
		conn:   nc,
		leader: sub(fmt.Sprintf(server.JSApiMsgGetT, stream)),
		direct: []*nats.Subscription{
			sub(fmt.Sprintf(server.JSDirectMsgGetT, stream)),
			sub(fmt.Sprintf(server.JSDirectGetLastBySubjectT, stream, ">")),
		},
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("register the counters: %v", err)
	}
	return w
}

func (w *wireReads) pending(t *testing.T, subs ...*nats.Subscription) int {
	t.Helper()
	if err := w.conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	total := 0
	for _, s := range subs {
		n, _, err := s.Pending()
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		total += n
	}
	return total
}

// leaderReads is how many leader reads the stream was sent.
func (w *wireReads) leaderReads(t *testing.T) int { t.Helper(); return w.pending(t, w.leader) }

// directReads is how many direct gets the stream was sent.
func (w *wireReads) directReads(t *testing.T) int { t.Helper(); return w.pending(t, w.direct...) }

// listScopes walks a budgets bucket and answers scope -> counter.
func listScopes(ctx context.Context, f *FleetStore, kv jetstream.KeyValue) (map[string]int, error) {
	out := map[string]int{}
	err := eachEntry(ctx, f.js, kv, func(kve jetstream.KeyValueEntry) error {
		r, err := decodeTally(kve.Value())
		if err != nil {
			return err
		}
		scope, _ := decodeKey(kve.Key())
		if _, twice := out[scope]; twice {
			return fmt.Errorf("scope %s visited twice", scope)
		}
		out[scope] = r.Slots[0].Used
		return nil
	})
	return out, err
}

// A KEY THE PASS NEVER DELIVERED IS READ BACK INTO THE LISTING.
//
// This is the defect the certification exists for, made deterministic. The
// pass's end marker is the client's guess — a pending count the broker
// decrements when an overwrite removes a revision and re-increments later, off
// a queue — so on a busy bucket the pass ends with a key it never delivered
// still in the stream. A listing that trusted the pass returned that key as
// ABSENT: a live node read as gone, a positions row that raises the trim floor.
// The stream's key index names it, and the listing reads it by itself — ONE
// read, for the one key the pass lost.
//
// And that read is the LEADER's. A direct get is what the bucket handle's own
// Get sends, and any replica may answer one from a copy that is behind — see
// TestACertifyingReadIsNeverAnsweredByAReplicaThatIsBehind for that failure on
// a real cluster. On one server the two reads return the same bytes, so what
// this case can hold on the regular runner is the request itself: not one
// direct get, ever.
func TestAKeyThePassNeverDeliveredIsReadBackIntoTheListing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	for i, scope := range []string{"org", "seat-a", "seat-b"} {
		putBudget(ctx, t, f.budgets, scope, 10*(i+1))
	}

	wire := countWireReads(t, nc, f.budgets)
	got, err := listScopes(ctx, f, probeKV{KeyValue: f.budgets, lose: encodeKey("seat-a")})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	want := map[string]int{"org": 10, "seat-a": 20, "seat-b": 30}
	if !maps.Equal(got, want) {
		t.Fatalf("listed %v, want %v: the key the pass lost is still in the "+
			"stream, and a listing that answers without it reports a live "+
			"record as absent", got, want)
	}
	if n := wire.leaderReads(t); n != 1 {
		t.Errorf("the listing sent the leader %d reads key by key, want exactly "+
			"1 — the one key the pass lost. More is a read per key, which is "+
			"the cost the one pass exists to avoid", n)
	}
	if n := wire.directReads(t); n != 0 {
		t.Errorf("the listing sent %d direct gets, want 0: a direct get is "+
			"answered by whichever replica the broker picks, and one that is "+
			"behind answers \"not found\" for the very key being certified", n)
	}
}

// AND A KEY THAT IS GONE BY THE TIME IT IS READ IS ABSENT, NOT AN ERROR.
//
// The index names every subject with a message, which includes a key whose
// newest message is a delete marker. When the pass lost such a key, the
// certifying read finds nothing — and that is an answer (the key is gone), not
// a store that could not be reached. Raising it would fail every listing that
// raced a delete.
func TestAKeyThePassLostThatIsGoneByItsReadIsAbsentRatherThanAnError(t *testing.T) {
	t.Parallel()
	for _, gone := range []struct {
		name   string
		remove func(context.Context, jetstream.KeyValue, string) error
	}{
		{"deleted", func(ctx context.Context, kv jetstream.KeyValue, key string) error {
			return kv.Delete(ctx, key)
		}},
		{"purged", func(ctx context.Context, kv jetstream.KeyValue, key string) error {
			return kv.Purge(ctx, key)
		}},
	} {
		t.Run(gone.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			nc := embeddedNATS(t)
			f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
			putBudget(ctx, t, f.budgets, "org", 1)
			putBudget(ctx, t, f.budgets, "seat-a", 2)
			if err := gone.remove(ctx, f.budgets, encodeKey("seat-a")); err != nil {
				t.Fatalf("remove: %v", err)
			}

			wire := countWireReads(t, nc, f.budgets)
			got, err := listScopes(ctx, f, probeKV{KeyValue: f.budgets, lose: encodeKey("seat-a")})
			if err != nil {
				t.Fatalf("a listing whose lost key had been %s failed: %v", gone.name, err)
			}
			if want := map[string]int{"org": 1}; !maps.Equal(got, want) {
				t.Fatalf("listed %v, want %v: the leader holds only a marker for "+
					"the lost key, and a marker is not a record", got, want)
			}
			if n := wire.leaderReads(t); n != 1 {
				t.Fatalf("made %d certifying reads, want 1: this case proves "+
					"nothing unless the removed key reached the read it is about", n)
			}
		})
	}
}

// replayKV is a REAL bucket whose pass is one RECORDED EARLIER — what a pass
// served by a replica that is behind delivers.
//
// The broker places a listing's pass on a random member of the stream's group
// and gives a caller no way to choose one, so "a replica that applied the
// delete but not the re-creation" cannot be arranged by picking the member.
// What such a pass DELIVERS can be: it is exactly the pass the bucket gave
// before the re-creation, and replaying that one to a listing taken after it
// hands the certification the real broker's marker — its revision, its
// headers, its timestamp — against a leader that has since moved on. The index
// read, the certifying read and the stream are all the broker's own.
type replayKV struct {
	jetstream.KeyValue
	pass []jetstream.KeyValueEntry
}

func (k replayKV) Watch(context.Context, string, ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	ch := make(chan jetstream.KeyValueEntry, len(k.pass)+1)
	for _, kve := range k.pass {
		ch <- kve
	}
	ch <- nil // the end of the initial values
	return stubWatcher{ch: ch}, nil
}

// recordPass is the pass kv delivers now, markers included, for a replayKV to
// hand to a listing taken later.
func recordPass(ctx context.Context, t *testing.T, kv jetstream.KeyValue) []jetstream.KeyValueEntry {
	t.Helper()
	w, err := kv.Watch(ctx, jetstream.AllKeys)
	if err != nil {
		t.Fatalf("record the pass: %v", err)
	}
	defer func() { _ = w.Stop() }()
	var pass []jetstream.KeyValueEntry
	for kve := range w.Updates() {
		if kve == nil {
			return pass
		}
		pass = append(pass, kve)
	}
	t.Fatal("the recorded pass ended without its end marker")
	return nil
}

// markerIn reports whether pass delivered key as a delete or purge marker —
// the premise of every case that replays one.
func markerIn(pass []jetstream.KeyValueEntry, key string) bool {
	for _, kve := range pass {
		if kve.Key() == key {
			return kve.Operation() != jetstream.KeyValuePut
		}
	}
	return false
}

// A TOMBSTONE A REPLICA SERVED FOR A LIVE KEY IS READ FROM THE LEADER.
//
// A key deleted and created again before the listing began, read by a pass on
// a member that had applied the delete and not yet the re-creation, comes back
// as a marker. A listing that took the marker as the key's answer dropped a
// key live throughout it — a secret the boot snapshot left out, a mailbox a
// sweep then treated as never registered. The index names the key, because the
// leader holds its value, and a marker in the pass is read again exactly as a
// key the pass never delivered is: ONE leader read, and not one direct get.
func TestATombstoneAReplicaServedForALiveKeyIsReadFromTheLeader(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	putBudget(ctx, t, f.budgets, "org", 1)
	putBudget(ctx, t, f.budgets, "seat-a", 2)
	if err := f.budgets.Delete(ctx, encodeKey("seat-a")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	behind := recordPass(ctx, t, f.budgets)
	if !markerIn(behind, encodeKey("seat-a")) {
		t.Fatalf("the recorded pass holds no marker for seat-a, so this case " +
			"replays nothing a replica that is behind would deliver")
	}
	putBudget(ctx, t, f.budgets, "seat-a", 3)

	wire := countWireReads(t, nc, f.budgets)
	got, err := listScopes(ctx, f, replayKV{KeyValue: f.budgets, pass: behind})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if want := map[string]int{"org": 1, "seat-a": 3}; !maps.Equal(got, want) {
		t.Fatalf("listed %v, want %v: seat-a was created again before the "+
			"listing began and is live throughout it, and the tombstone a "+
			"replica that is behind served for it is not its answer", got, want)
	}
	if n := wire.leaderReads(t); n != 1 {
		t.Errorf("the listing sent the leader %d reads, want exactly 1 — the one "+
			"key the pass delivered as a marker", n)
	}
	if n := wire.directReads(t); n != 0 {
		t.Errorf("the listing sent %d direct gets, want 0: a direct get is "+
			"answered by a replica, which is the very copy the marker came from", n)
	}
}

// A TOMBSTONE COSTS ONE LEADER READ UNTIL IT IS SWEPT, and a quiet bucket with
// none costs no read at all.
//
// The cost half of the certification's promise. A marker the pass delivers is
// read again from the leader, so every marker a bucket keeps is a read on
// every listing that meets it — and a bucket the broker never ages keeps each
// one for the life of the deployment unless the sweep removes it. Before the
// sweep, three markers are three reads; after it, the same listing reads
// nothing, which is what bounds the term by the records removed recently
// rather than by every record ever removed.
//
// ON THE CHANNELS BUCKET, which has no age and removes a record as each ask
// closes. The values are a counter's because the listing helper reads them,
// and neither the walk nor the sweep reads a value at all. (The budgets bucket
// these were first written over now has an age, and the broker ages its
// markers with it: the fleet's sweep rightly leaves it alone.)
func TestATombstoneCostsOneLeaderReadUntilItIsSwept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	for _, scope := range []string{"org", "seat-a", "seat-b", "gone-1", "gone-2", "purged"} {
		putBudget(ctx, t, f.channels, scope, 1)
	}
	for _, scope := range []string{"gone-1", "gone-2"} {
		if err := f.channels.Delete(ctx, encodeKey(scope)); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	if err := f.channels.Purge(ctx, encodeKey("purged")); err != nil {
		t.Fatalf("purge: %v", err)
	}
	want := map[string]int{"org": 1, "seat-a": 1, "seat-b": 1}

	wire := countWireReads(t, nc, f.channels)
	got, err := listScopes(ctx, f, f.channels)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	if n := wire.leaderReads(t); n != 3 {
		t.Errorf("a listing over three markers made %d leader reads, want 3 — "+
			"one per marker, each asked of the leader rather than trusted", n)
	}

	swept, err := f.SweepMarkers(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 3 {
		t.Fatalf("the sweep removed %d markers, want the 3 the bucket held", swept)
	}
	wire = countWireReads(t, nc, f.channels)
	got, err = listScopes(ctx, f, f.channels)
	if err != nil {
		t.Fatalf("listing after the sweep: %v", err)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("listed %v after the sweep, want %v: removing the markers "+
			"changed the answer", got, want)
	}
	if n := wire.leaderReads(t) + wire.directReads(t); n != 0 {
		t.Errorf("a listing of a swept bucket nobody was writing made %d reads "+
			"key by key, want 0", n)
	}
}

// A LISTING THAT CANNOT BE CERTIFIED IS NOT A LISTING.
//
// The pass alone cannot tell a complete answer from one that lost a key, so a
// failure of either certifying read is the third answer, named as the listing
// that could not finish — never the pass's answer handed over as if it were
// whole, and never a partial visit before the error.
func TestAListingThatCannotBeCertifiedIsUnavailable(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)

	t.Run("the_key_index_cannot_be_read", func(t *testing.T) {
		t.Parallel()
		f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
		ctx := context.Background()
		putBudget(ctx, t, f.budgets, "org", 1)
		// A client that cannot reach the stream's index: it addresses the
		// embedded fleet's domain, which this raw broker does not serve, so
		// every JetStream API request finds no responder — while the
		// bucket's own pass goes through the handle it already holds.
		blind, err := jsapi.Embedded().Client(nc)
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		shortCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var visited int
		err = eachEntryUnder(shortCtx, blind, f.budgets, jetstream.AllKeys, "the budgets",
			func(jetstream.KeyValueEntry) error { visited++; return nil })
		if !errors.Is(err, coord.ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable: a pass nobody could certify "+
				"was handed over as the whole bucket", err)
		}
		if !strings.Contains(err.Error(), "the budgets") || !strings.Contains(err.Error(), "key index") {
			t.Errorf("err = %q, want it to name the listing and the index read "+
				"that failed", err)
		}
		if visited != 0 {
			t.Errorf("visited %d entries of a listing that failed", visited)
		}
	})

	t.Run("a_certifying_read_fails", func(t *testing.T) {
		t.Parallel()
		f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
		ctx := context.Background()
		putBudget(ctx, t, f.budgets, "org", 1)
		putBudget(ctx, t, f.budgets, "seat-a", 1)
		// The index goes through the working client and the certifying read
		// through one whose API no responder serves: the read the listing
		// needs is the one that cannot be made.
		blind, err := jsapi.Embedded().Client(nc)
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		read, err := newLeaderReader(blind, f.budgets)
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		var visited int
		err = listUnder(ctx, f.js, read, probeKV{KeyValue: f.budgets, lose: encodeKey("seat-a")},
			jetstream.AllKeys, "the budgets",
			func(jetstream.KeyValueEntry) error { visited++; return nil })
		if !errors.Is(err, coord.ErrUnavailable) || !errors.Is(err, nats.ErrNoResponders) {
			t.Fatalf("err = %v, want ErrUnavailable wrapping the read's own "+
				"failure: the key it could not read may well be live", err)
		}
		if !strings.Contains(err.Error(), "the budgets") || !strings.Contains(err.Error(), encodeKey("seat-a")) {
			t.Errorf("err = %q, want it to name the listing and the key it could not read", err)
		}
		if visited != 0 {
			t.Errorf("visited %d entries of a listing that failed", visited)
		}
	})
}

// churnFor runs write in tight loops, one goroutine per key, until the
// returned stop is called; stop reports how many writes landed and the first
// failure. A write that fails ends its loop, because the case asserts about
// keys that were live THROUGHOUT and a failed renew is a key that may not be.
func churnFor(t *testing.T, keys []string, write func(key string) error) (stop func() (int64, error)) {
	t.Helper()
	var (
		wg     sync.WaitGroup
		writes atomic.Int64
		mu     sync.Mutex
		first  error
	)
	done := make(chan struct{})
	for _, key := range keys {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				if err := write(key); err != nil {
					mu.Lock()
					if first == nil {
						first = fmt.Errorf("%s: %w", key, err)
					}
					mu.Unlock()
					return
				}
				writes.Add(1)
			}
		})
	}
	return func() (int64, error) {
		close(done)
		wg.Wait()
		return writes.Load(), first
	}
}

// churnListings is how many listings a churn case takes. Measured before the
// certification existed, 23% of ListLive calls against three tight renewers
// lost a live lease, so this many passes lose one with certainty; afterwards
// every one of them must be whole.
const churnListings = 300

// A LEASE RENEWED THROUGHOUT A LISTING IS IN IT. The regression.
//
// Every membership read is ListLive: seat placement divides the company by
// ListLive(ClassNode), the object map is built from ListLive(ClassObjects). A
// renew is an overwrite, and on a bucket keeping one revision per key an
// overwrite of a key the pass has not reached removes the revision it was
// about to deliver — measured at 848 misses in 3634 listings, each a live
// node that looked gone to the node reading.
//
// The listing runs on its OWN store over the same buckets, with its lease
// bucket counted, so the case can prove the race actually happened: a run in
// which no listing needed a certifying read is a run in which the renewals
// never overlapped a pass, and it would pass whatever the walk did.
func TestAListingNeverMissesALeaseBeingRenewed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("r%d", bucketSeq.Add(1))
	open := func() *Store {
		s, err := Open(ctx, jsOf(nc), Config{TTL: time.Minute, BucketPrefix: prefix})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return s
	}
	writer, reader := open(), open()
	wire := countWireReads(t, nc, reader.leases.kv)

	nodes := []string{"n0", "n1", "n2", "n3", "n4"}
	leases := map[string]*coord.Lease{}
	for _, n := range nodes {
		l, _, err := writer.TryAcquire(ctx, coord.NodeResource(n), coord.AcquireOptions{
			Owner: n + ":1", TTL: time.Minute, Ungated: true,
		})
		if err != nil || l == nil {
			t.Fatalf("claim %s: (%v, %v)", n, l, err)
		}
		leases[n] = l
	}

	stop := churnFor(t, []string{"n0", "n2", "n4"}, func(n string) error {
		l := leases[n]
		held, err := writer.Renew(ctx, l.Resource, l.Owner, l.Epoch, time.Minute)
		if err == nil && !held {
			err = errors.New("the renew reported the lease lost")
		}
		return err
	})
	var misses []string
	for range churnListings {
		live, err := reader.ListLive(ctx, coord.ClassNode)
		if err != nil {
			_, _ = stop()
			t.Fatalf("ListLive: %v", err)
		}
		if got := resourcesOf(live); !slices.Equal(got, []string{
			"node:n0", "node:n1", "node:n2", "node:n3", "node:n4",
		}) && len(misses) < 5 {
			misses = append(misses, strings.Join(got, ","))
		}
	}
	renewals, err := stop()
	if err != nil {
		t.Fatalf("a renewer stopped, so its lease was not live throughout: %v", err)
	}
	if len(misses) > 0 {
		t.Fatalf("ListLive(ClassNode) answered without a lease renewed "+
			"throughout the listing; first answers: %v", misses)
	}
	if wire.leaderReads(t) == 0 {
		t.Fatalf("%d renewals across %d listings and not one listing needed a "+
			"certifying read, so the renewals never raced a pass and this run "+
			"proves nothing about one that does", renewals, churnListings)
	}
}

func resourcesOf(leases []coord.Lease) []string {
	out := make([]string, 0, len(leases))
	for _, l := range leases {
		out = append(out, l.Resource)
	}
	return out
}

// A POSITIONS ROW HEARTBEATED THROUGHOUT A LISTING IS IN IT, ONCE.
//
// The same walk under the FleetStore's listings, and the sharpest of them: the
// trim takes a MINIMUM across these rows, so a row the listing misses raises
// the floor and deletes log records its node has not applied. And the pass can
// deliver one row at two revisions, which every appending listing used to
// return twice — two rows for one node, one of them stale.
func TestAPositionsListingNeverMissesOrRepeatsARowBeingHeartbeated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("r%d", bucketSeq.Add(1))
	writer, reader := openFleetForTest(t, nc, prefix), openFleetForTest(t, nc, prefix)
	wire := countWireReads(t, nc, reader.positions)

	nodes := []string{"n0", "n1", "n2", "n3", "n4"}
	var seq atomic.Uint64
	heartbeat := func(n string) error {
		s := seq.Add(1)
		return writer.PutPositions(ctx, coord.NodePositions{
			NodeID:  n,
			Domains: map[string]coord.DomainPosition{"tracker": {Seq: s, AppliedThrough: s}},
		})
	}
	for _, n := range nodes {
		if err := heartbeat(n); err != nil {
			t.Fatalf("PutPositions %s: %v", n, err)
		}
	}

	stop := churnFor(t, []string{"n0", "n2", "n4"}, heartbeat)
	var wrong []string
	for range churnListings {
		rows, err := reader.Positions(ctx)
		if err != nil {
			_, _ = stop()
			t.Fatalf("Positions: %v", err)
		}
		got := make([]string, 0, len(rows))
		for _, r := range rows {
			got = append(got, r.NodeID)
		}
		slices.Sort(got)
		if !slices.Equal(got, nodes) && len(wrong) < 5 {
			wrong = append(wrong, strings.Join(got, ","))
		}
	}
	beats, err := stop()
	if err != nil {
		t.Fatalf("a heartbeat failed: %v", err)
	}
	if len(wrong) > 0 {
		t.Fatalf("Positions did not answer every node exactly once while three "+
			"of them heartbeated; first answers: %v", wrong)
	}
	if wire.leaderReads(t) == 0 {
		t.Fatalf("%d heartbeats across %d listings and not one listing needed a "+
			"certifying read, so this run proves nothing about a pass that races "+
			"one", beats, churnListings)
	}
}

// A LEADER READ DECODES A KEY EXACTLY AS THE CLIENT'S OWN READS DO.
//
// The certifying read goes past the client, so it decodes the stored message
// itself — and an entry it decoded differently from the pass would list a key
// the pass would have dropped, or drop one it would have listed. On one server
// the client's direct get is exact, so the two must agree field by field: a
// live key's value, revision and time, a delete marker and a purge marker as
// what they are, and a key that was never written as not found.
func TestALeaderReadDecodesAKeyExactlyAsTheClientDoes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openFleetForTest(t, embeddedNATS(t), fmt.Sprintf("f%d", bucketSeq.Add(1)))
	putBudget(ctx, t, f.budgets, "live", 7)
	putBudget(ctx, t, f.budgets, "deleted", 1)
	putBudget(ctx, t, f.budgets, "purged", 1)
	if err := f.budgets.Delete(ctx, encodeKey("deleted")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := f.budgets.Purge(ctx, encodeKey("purged")); err != nil {
		t.Fatalf("purge: %v", err)
	}
	read, err := newLeaderReader(f.js, f.budgets)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}

	// The client's own view of each key: its Get for a live one, and its
	// watcher for a marker, since Get folds every marker into "not found"
	// and the operation is the part that has to agree.
	clientView := func(key string) jetstream.KeyValueEntry {
		t.Helper()
		w, err := f.budgets.Watch(ctx, key)
		if err != nil {
			t.Fatalf("watch %s: %v", key, err)
		}
		defer func() { _ = w.Stop() }()
		kve := <-w.Updates()
		if kve == nil {
			t.Fatalf("the client's watcher holds nothing for %s", key)
		}
		return kve
	}

	for _, scope := range []string{"live", "deleted", "purged"} {
		key := encodeKey(scope)
		got, err := read.last(ctx, key)
		if err != nil {
			t.Fatalf("leader read of %s: %v", scope, err)
		}
		want := clientView(key)
		if got.Operation() != want.Operation() || got.Revision() != want.Revision() ||
			got.Key() != want.Key() || got.Bucket() != want.Bucket() ||
			string(got.Value()) != string(want.Value()) || !got.Created().Equal(want.Created()) {
			t.Errorf("%s: the leader read decoded {%v rev %d key %q bucket %q value %q at %v}, "+
				"the client {%v rev %d key %q bucket %q value %q at %v}", scope,
				got.Operation(), got.Revision(), got.Key(), got.Bucket(), got.Value(), got.Created(),
				want.Operation(), want.Revision(), want.Key(), want.Bucket(), want.Value(), want.Created())
		}
	}
	if kve, err := f.budgets.Get(ctx, encodeKey("live")); err != nil || string(kve.Value()) != string(clientView(encodeKey("live")).Value()) {
		t.Fatalf("the client's Get and watcher disagree about a quiet key: (%v, %v)", kve, err)
	}
	if got, err := read.last(ctx, encodeKey("never-written")); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Errorf("a key with no message read as (%v, %v), want ErrKeyNotFound — "+
			"the answer certify reads as absent rather than as a failure", got, err)
	}
}

// A LISTING'S LEADER READ IS ADDRESSED WHERE ITS CLIENT'S OWN REQUESTS GO.
//
// The read goes past the client, so it has to be addressed in the API the
// client speaks or nothing answers it: a leaf's broker serves JetStream only
// under the embedded fleet's domain. This package is handed a client, so it
// recovers the API from it — and a client built any way but internal/jsapi
// is a wiring mistake named before the broker is asked anything, rather than
// a certifying read that fails only on the listings that raced a write.
func TestALeaderReadIsAddressedInTheAPIItsClientSpeaks(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	must := func(js jetstream.JetStream, err error) jetstream.JetStream {
		t.Helper()
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		return js
	}
	for _, c := range []struct {
		name   string
		client jetstream.JetStream
		want   jsapi.API
		refuse string
	}{
		{"the_account", must(jsapi.Account().Client(nc)), jsapi.Account(), ""},
		{"the_embedded_fleet", must(jsapi.Embedded().Client(nc)), jsapi.Embedded(), ""},
		{"a_custom_prefix", must(jetstream.NewWithAPIPrefix(nc, "$ELSEWHERE.API")), jsapi.API{}, "$ELSEWHERE.API"},
		{"another_domain", must(jetstream.NewWithDomain(nc, "elsewhere")), jsapi.API{}, "elsewhere"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := apiOf(c.client)
			if c.refuse != "" {
				if err == nil || !strings.Contains(err.Error(), c.refuse) || !strings.Contains(err.Error(), "internal/jsapi") {
					t.Fatalf("apiOf = (%v, %v), want a refusal naming %q and internal/jsapi", got, err, c.refuse)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("apiOf = (%v, %v), want %v", got, err, c.want)
			}
		})
	}

	t.Run("a_misbuilt_client_fails_every_listing", func(t *testing.T) {
		f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
		putBudget(context.Background(), t, f.budgets, "org", 1)
		elsewhere := must(jetstream.NewWithDomain(nc, "elsewhere"))
		err := eachEntry(context.Background(), elsewhere, f.budgets,
			func(jetstream.KeyValueEntry) error { return nil })
		if err == nil || errors.Is(err, coord.ErrUnavailable) {
			t.Fatalf("err = %v, want a wiring error that is NOT ErrUnavailable: "+
				"nothing about this client improves on a retry", err)
		}
	})
}

// embeddedNATSServing is [embeddedNATS] for a broker that serves its JetStream
// under a domain, which is how every member of the engine's embedded fleet
// runs.
func embeddedNATSServing(t *testing.T, domain string) *nats.Conn {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		ServerName:      "coordkv-domain-test",
		JetStream:       true,
		JetStreamDomain: domain,
		Port:            -1,
		DontListen:      true,
		StoreDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("configure embedded server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(30 * time.Second) {
		ns.Shutdown()
		t.Fatal("embedded nats server did not become ready")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("connect to embedded server: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// AND ON THE EMBEDDED FLEET'S OWN DOMAIN THE READ IS ANSWERED.
//
// The case above holds which API is chosen; this one holds the address it
// produces, end to end, on a broker serving the domain every member of the
// fleet serves — the only address a leaf's client has, since a leaf runs no
// JetStream of its own. A leader read addressed anywhere else on that client
// finds no responder, so a key the pass lost could never be certified and
// every listing that raced a write would fail. (A member answers its account's
// API as well, which is why the choice itself is held by the case above rather
// than here.)
func TestAKeyThePassLostIsReadBackOnTheEmbeddedFleetsDomain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATSServing(t, jsapi.Domain)
	js, err := jsapi.Embedded().Client(nc)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	store, err := OpenFleet(ctx, js, FleetConfig{
		BucketPrefix: fmt.Sprintf("d%d", bucketSeq.Add(1)),
		RateWindow:   time.Minute, ClaimTTL: 10 * time.Minute,
		LedgerRetention: 10 * time.Minute, FireRetention: 10 * time.Minute,
		FollowRetention: 10 * time.Minute, BudgetRetention: time.Minute, CooldownMax: time.Hour,
		RebaseRetention: 10 * time.Minute,
		StatusFreshness: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("OpenFleet: %v", err)
	}
	putBudget(ctx, t, store.budgets, "org", 1)
	putBudget(ctx, t, store.budgets, "seat-a", 2)

	got, err := listScopes(ctx, store, probeKV{KeyValue: store.budgets, lose: encodeKey("seat-a")})
	if err != nil {
		t.Fatalf("a listing on the fleet's domain whose pass lost a key: %v", err)
	}
	if want := map[string]int{"org": 1, "seat-a": 2}; !maps.Equal(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
}
