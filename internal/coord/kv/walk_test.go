package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// truncatingKV is a bucket whose listing STOPS WITHOUT SAYING SO.
//
// Embedding the interface rather than implementing it: only WatchAll and
// Bucket are reached, and a method this test does not mean to exercise should
// panic rather than quietly answer a zero value.
type truncatingKV struct {
	jetstream.KeyValue
	deliver int // entries handed over before the channel closes
}

func (k truncatingKV) Bucket() string { return "truncating" }

func (k truncatingKV) WatchAll(context.Context, ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
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
			err := watchWalk(context.Background(), truncatingKV{deliver: delivered},
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
		CooldownMax:     time.Hour,
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

	got, err := store.Usage(context.Background())
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
	good, err := json.Marshal(budgetRecord{Used: 1, At: time.Now().UTC()})
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
	if err := watchWalk(ctx, store.budgets, func(jetstream.KeyValueEntry) error {
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

// A BUCKET READ ASKS FOR NO CONSUMER AT ALL.
//
// The point of the batched read, stated as the thing an operator would see: a
// walk is one request/reply and the consumer API is never touched. Asserting
// the stream's consumer count afterwards would not say this — the ordered walk
// stops its watcher too, so that count is zero either way. What separates them
// is whether a CONSUMER.CREATE ever went out, which on a clustered bucket is
// two metadata-raft proposals per walk, paid by five duty loops every fifteen
// seconds and by the state-log write fence on every first write to a subject.
func TestABucketReadIssuesNoConsumerRequest(t *testing.T) {
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
	store := openFleetForTest(t, nc, prefix)
	ctx := context.Background()

	good, err := json.Marshal(budgetRecord{Used: 7, At: time.Now().UTC()})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := range 5 {
		if _, err := store.budgets.Put(ctx, encodeKey(fmt.Sprintf("scope-%d", i)), good); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	// Subscribed BEFORE the walk, on the same connection the walk uses, so
	// its own requests echo back here.
	var consumerAPI atomic.Int64
	watch, err := nc.Subscribe("$JS.API.CONSUMER.>", func(*nats.Msg) { consumerAPI.Add(1) })
	if err != nil {
		t.Fatalf("watch the consumer API: %v", err)
	}
	defer func() { _ = watch.Unsubscribe() }()
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	usage, err := store.Usage(ctx)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(usage) != 5 {
		t.Fatalf("Usage listed %d scopes, want 5", len(usage))
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n := consumerAPI.Load(); n != 0 {
		t.Errorf("reading a bucket made %d consumer API call(s); the batched read "+
			"exists precisely so a walk costs no consumer", n)
	}
}

// THE TWO TRANSPORTS ANSWER THE SAME BUCKET.
//
// One contract, two backends, one suite is this package's own rule for a queue
// — and a walk now has two transports under [eachEntry], so nothing above it
// can tell which one answered. That is only true while they agree about every
// part of an entry a caller reads, and about which keys are ALIVE: the ordered
// walk has the broker drop tombstones for it ([jetstream.IgnoreDeletes]) and
// the batched read has to recognise them itself, in both the spellings the
// broker writes.
func TestTheTwoTransportsAgreeAboutABucket(t *testing.T) {
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
	store := openFleetForTest(t, nc, prefix)
	ctx := context.Background()

	for i := range 12 {
		if _, err := store.budgets.Put(ctx, fmt.Sprintf("live-%02d", i), fmt.Appendf(nil, `{"n":%d}`, i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	// A delete and a purge: two tombstone spellings, neither of which is a
	// key either transport may report.
	if _, err := store.budgets.Put(ctx, "gone-deleted", []byte(`{"n":-1}`)); err != nil {
		t.Fatalf("seed the deleted key: %v", err)
	}
	if err := store.budgets.Delete(ctx, "gone-deleted"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.budgets.Put(ctx, "gone-purged", []byte(`{"n":-2}`)); err != nil {
		t.Fatalf("seed the purged key: %v", err)
	}
	if err := store.budgets.Purge(ctx, "gone-purged"); err != nil {
		t.Fatalf("purge: %v", err)
	}

	collect := func(walk func(func(jetstream.KeyValueEntry) error) error) map[string]string {
		t.Helper()
		out := map[string]string{}
		if err := walk(func(kve jetstream.KeyValueEntry) error {
			if _, seen := out[kve.Key()]; seen {
				t.Errorf("key %q was visited twice in one walk", kve.Key())
			}
			out[kve.Key()] = fmt.Sprintf("%s@%d", kve.Value(), kve.Revision())
			return nil
		}); err != nil {
			t.Fatalf("walk: %v", err)
		}
		return out
	}

	ordered := collect(func(visit func(jetstream.KeyValueEntry) error) error {
		return watchWalk(ctx, store.budgets, visit)
	})
	batched := collect(func(visit func(jetstream.KeyValueEntry) error) error {
		declined, err := directWalk(ctx, nc, store.budgets, directWalkMaxBytes, visit)
		if declined {
			t.Fatal("this broker declined a batched read; the embedded one is what " +
				"this package is measured against and it is well past 2.11")
		}
		return err
	})

	if len(ordered) != 12 {
		t.Fatalf("the ordered walk reported %d live keys, want 12 — a tombstone leaked", len(ordered))
	}
	if !maps.Equal(ordered, batched) {
		t.Errorf("the two transports disagree about the bucket:\n ordered = %v\n batched = %v",
			ordered, batched)
	}
}

// A BATCH THAT DOES NOT FIT IS PAGED, NEVER TRUNCATED.
//
// The broker stops a batch once it has sent max_bytes and says how many
// records it still holds. Reading that as the end of the bucket is the short
// listing this package refuses; the continuation asks for the SAME snapshot
// the first batch pinned, so a concurrent write cannot make the walk skip a
// key or hand one over twice.
func TestABatchedReadPagesRatherThanTruncating(t *testing.T) {
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
	store := openFleetForTest(t, nc, prefix)
	ctx := context.Background()

	const keys = 40
	for i := range keys {
		if _, err := store.budgets.Put(ctx, fmt.Sprintf("k-%02d", i), fmt.Appendf(nil, `{"n":%d}`, i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	// One byte, so the broker's own "have I sent enough" check trips on
	// every record and each batch carries exactly one. Forty pages is the
	// resume path exercised forty times, for a fraction of the data eight
	// megabytes would take to write.
	seen := map[string]int{}
	declined, err := directWalk(ctx, nc, store.budgets, 1, func(kve jetstream.KeyValueEntry) error {
		seen[kve.Key()]++
		return nil
	})
	if err != nil {
		t.Fatalf("directWalk: %v", err)
	}
	if declined {
		t.Fatal("the embedded broker declined a batched read")
	}
	if len(seen) != keys {
		t.Errorf("a paged walk visited %d of %d keys", len(seen), keys)
	}
	for key, times := range seen {
		if times != 1 {
			t.Errorf("key %q was visited %d times; a resumed batch must not replay", key, times)
		}
	}
}

// --- what a broker that cannot answer a batched read is allowed to do ------

// ghostBucket names a bucket nothing created, so a case can put a fake broker
// on its direct endpoint and hand directWalk exactly the replies it means to
// test. Embedding the interface rather than implementing it: only Bucket is
// reached, and a method a case does not mean to exercise should panic rather
// than quietly answer a zero value.
type ghostBucket struct {
	jetstream.KeyValue
	bucket string
}

func (b ghostBucket) Bucket() string { return b.bucket }

// answerBatchedReads puts a fake broker on one bucket's direct endpoint.
//
// reply is handed the request's round number, one-based, and the subject to
// answer on — so a case can say "a record, then the refusal" as plainly as it
// says "the refusal".
func answerBatchedReads(t *testing.T, nc *nats.Conn, reply func(round int, to string)) ghostBucket {
	t.Helper()
	bucket := fmt.Sprintf("ghost%d", bucketSeq.Add(1))
	var round atomic.Int64
	sub, err := nc.Subscribe(fmt.Sprintf(server.JSDirectMsgGetT, kvStreamPrefix+bucket),
		func(msg *nats.Msg) { reply(int(round.Add(1)), msg.Reply) })
	if err != nil {
		t.Fatalf("stand up a fake broker: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush the fake broker's interest: %v", err)
	}
	return ghostBucket{bucket: bucket}
}

// sendStatus answers with one of the broker's status-only replies.
func sendStatus(t *testing.T, nc *nats.Conn, to, status, description string) {
	t.Helper()
	msg := nats.NewMsg(to)
	msg.Header.Set(statusHeader, status)
	if description != "" {
		msg.Header.Set(descriptionHeader, description)
	}
	if err := nc.PublishMsg(msg); err != nil {
		t.Errorf("answer %s: %v", status, err)
	}
}

// sendRecord answers with one record of a batch.
func sendRecord(t *testing.T, nc *nats.Conn, to, subject string, seq, pending uint64, value string) {
	t.Helper()
	msg := nats.NewMsg(to)
	msg.Header.Set(server.JSSubject, subject)
	msg.Header.Set(server.JSSequence, strconv.FormatUint(seq, 10))
	msg.Header.Set(server.JSTimeStamp, time.Now().UTC().Format(time.RFC3339Nano))
	msg.Header.Set(server.JSNumPending, strconv.FormatUint(pending, 10))
	msg.Data = []byte(value)
	if err := nc.PublishMsg(msg); err != nil {
		t.Errorf("answer with a record: %v", err)
	}
}

// A BROKER THAT CANNOT SERVE A BATCHED READ DECLINES; IT DOES NOT FAIL.
//
// Three deployments reach this, and none of them is broken: a cluster older
// than the 2.11.0 that introduced multi_last answers 408 because it parsed a
// request with nothing it recognises set; one that gates on API level answers
// 412; and a bucket past the broker's 1024-subject ceiling answers 413. Every
// one of them is raised before a record is sent, so the walk starts over on
// the ordered transport rather than reporting an outage that is not happening.
func TestABrokerThatCannotAnswerABatchedReadDeclines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, status, description string
	}{
		{"older_than_multi_last", statusBadRequest, "Empty Request"},
		{"gated_on_api_level", statusRequiredAPI, "Required Api Level"},
		{"more_subjects_than_one_batch", statusTooManyResults, "Too Many Results"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			nc := embeddedNATS(t)
			bucket := answerBatchedReads(t, nc, func(_ int, to string) {
				sendStatus(t, nc, to, tc.status, tc.description)
			})

			declined, err := directWalk(context.Background(), nc, bucket, directWalkMaxBytes,
				func(jetstream.KeyValueEntry) error {
					t.Error("a declined read visited an entry")
					return nil
				})
			if err != nil {
				t.Fatalf("a %s answer came back as an error (%v); it is a fact about the "+
					"deployment, and the walk has another transport for it", tc.status, err)
			}
			if !declined {
				t.Fatalf("a %s answer was not reported as a decline, so the walk would "+
					"report an empty bucket instead of reading it", tc.status)
			}
		})
	}
}

// AND A DIRECT ENDPOINT NOBODY IS SERVING IS THE SAME KIND OF FACT.
//
// A batched read is served off the stream's DIRECT endpoint, which exists only
// where AllowDirect is set — and openBucket deliberately OBSERVES a bucket
// that already exists rather than rewriting its configuration, so a bucket
// adopted from an older client can be missing it. The broker answers 503 with
// no responder, which is not an outage: the stream is there and the other
// transport reads it.
func TestADirectEndpointNobodyServesDeclines(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)

	declined, err := directWalk(context.Background(), nc,
		ghostBucket{bucket: fmt.Sprintf("unserved%d", bucketSeq.Add(1))}, directWalkMaxBytes,
		func(jetstream.KeyValueEntry) error {
			t.Error("a read nobody answered visited an entry")
			return nil
		})
	if err != nil {
		t.Fatalf("no responder came back as an error: %v", err)
	}
	if !declined {
		t.Fatal("no responder was not reported as a decline, so the walk would report " +
			"an empty bucket instead of reading it")
	}
}

// A BATCHED READ THAT STOPS WITHOUT ITS END-OF-BATCH MARKER IS UNAVAILABLE,
// NEVER A SHORT BUCKET.
//
// The same rule the ordered walk keeps with the nil end-of-values marker, on
// the transport that has a different one. A broker that sent some of the
// bucket and then stopped answering has not told us the bucket is that size,
// and a caller that read it as one would act on a bucket it never saw.
func TestABatchedReadWithoutItsMarkerIsUnavailable(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	var bucket ghostBucket
	bucket = answerBatchedReads(t, nc, func(_ int, to string) {
		sendRecord(t, nc, to, kvSubjectPrefix+bucket.bucket+".one", 1, 3, `{"n":1}`)
		// ...and then nothing. No marker, no status, no further records.
	})

	// The caller's own deadline rather than directReplyTimeout, so the case
	// measures the rule in milliseconds instead of waiting out the timeout
	// that bounds a real broker.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	var seen int
	declined, err := directWalk(ctx, nc, bucket, directWalkMaxBytes,
		func(jetstream.KeyValueEntry) error { seen++; return nil })
	if err == nil {
		t.Fatalf("a read that stopped after %d of an unknown number of records returned "+
			"no error; the caller reads that as the whole bucket", seen)
	}
	if !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("error = %v, want it to carry coord.ErrUnavailable so the caller can "+
			"tell it from an empty bucket", err)
	}
	if declined {
		t.Error("a read that had already handed over a record called itself a decline, " +
			"so the walk would start again on the other transport and visit it twice")
	}
	if seen != 1 {
		t.Errorf("visited %d records, want the 1 delivered before the cut", seen)
	}
}

// AND A DECLINE THAT ARRIVES AFTER A RECORD IS AN ERROR RATHER THAN A RESTART.
//
// The fallback is only ever reachable while nothing has been visited — a walk
// that starts again on the other transport after handing over entries hands
// the caller those entries twice. The broker raises all three declines before
// it sends a record, so this cannot happen against a healthy one; what makes
// it reachable is a cluster changing under a paged read, and the honest answer
// there is that the walk could not be completed.
func TestADeclineAfterARecordIsAnErrorRatherThanARestart(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	var bucket ghostBucket
	bucket = answerBatchedReads(t, nc, func(round int, to string) {
		if round == 1 {
			sendRecord(t, nc, to, kvSubjectPrefix+bucket.bucket+".one", 1, 1, `{"n":1}`)
			msg := nats.NewMsg(to)
			msg.Header.Set(statusHeader, statusEndOfBatch)
			msg.Header.Set(server.JSNumPending, "1")
			msg.Header.Set(server.JSUpToSequence, "9")
			msg.Header.Set(server.JSLastSequence, "1")
			if err := nc.PublishMsg(msg); err != nil {
				t.Errorf("answer with a marker: %v", err)
			}
			return
		}
		// The continuation finds a broker that will not serve it.
		sendStatus(t, nc, to, statusTooManyResults, "Too Many Results")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var seen int
	declined, err := directWalk(ctx, nc, bucket, directWalkMaxBytes,
		func(jetstream.KeyValueEntry) error { seen++; return nil })
	if err == nil {
		t.Fatal("a read abandoned half way came back clean; the caller reads that as " +
			"the whole bucket")
	}
	if !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("error = %v, want coord.ErrUnavailable", err)
	}
	if declined {
		t.Error("the walk called itself a decline after visiting a record, so the " +
			"ordered transport would run and visit it a second time")
	}
	if seen != 1 {
		t.Errorf("visited %d records, want the 1 the first batch carried", seen)
	}
}

// A BROKER THAT SAYS THERE IS MORE AND WILL NOT SAY WHERE IT STARTS IS
// UNAVAILABLE, NOT A LOOP.
//
// A continuation asks for the sequence after the last record of the previous
// batch, within the snapshot that batch pinned. Missing either, the only two
// things this could do are ask the same question again for ever, or report the
// records it happened to get as the whole bucket — an unbounded read, or the
// short listing this package refuses. It reports neither.
func TestAContinuationWithNothingToContinueFromIsUnavailable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		upTo    string
		lastSeq func(round int) string
	}{
		{"no_snapshot_to_continue_from", "", func(int) string { return "1" }},
		{"no_progress_between_batches", "9", func(int) string { return "0" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			nc := embeddedNATS(t)
			var bucket ghostBucket
			bucket = answerBatchedReads(t, nc, func(round int, to string) {
				sendRecord(t, nc, to, kvSubjectPrefix+bucket.bucket+".one", 1, 1, `{"n":1}`)
				msg := nats.NewMsg(to)
				msg.Header.Set(statusHeader, statusEndOfBatch)
				// More to come, always.
				msg.Header.Set(server.JSNumPending, "1")
				if tc.upTo != "" {
					msg.Header.Set(server.JSUpToSequence, tc.upTo)
				}
				msg.Header.Set(server.JSLastSequence, tc.lastSeq(round))
				if err := nc.PublishMsg(msg); err != nil {
					t.Errorf("answer with a marker: %v", err)
				}
			})

			// Generous, and never reached if the guard holds — what it
			// bounds is the failure, which is an unbounded loop.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			declined, err := directWalk(ctx, nc, bucket, directWalkMaxBytes,
				func(jetstream.KeyValueEntry) error { return nil })
			if err == nil {
				t.Fatal("a read the broker would not let us finish came back clean")
			}
			if !errors.Is(err, coord.ErrUnavailable) {
				t.Errorf("error = %v, want coord.ErrUnavailable", err)
			}
			// THE WALK REFUSES; THE DEADLINE DOES NOT REFUSE FOR IT.
			// Without this the case passes either way — an unguarded
			// loop asks the same question until the context expires,
			// and that answer is coord.ErrUnavailable too. What is
			// being asserted is that the walk stopped on what the
			// broker said rather than on running out of time.
			if errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("the walk ran until its deadline (%v) instead of refusing a "+
					"continuation it had nowhere to start; that is an unbounded read "+
					"of one batch", err)
			}
			if declined {
				t.Error("the walk called itself a decline after visiting a record")
			}
		})
	}
}

// AND THEY AGREE ABOUT THE TOMBSTONE NEITHER OF THEM WROTE.
//
// A broker that reaps a key on its own marks the gap with Nats-Marker-Reason
// rather than the KV-Operation a caller's own delete carries — a second
// spelling, produced by nobody in this package. No bucket here asks for those
// markers today, which is exactly why this case exists: the ordered walk gets
// them dropped for it by the broker, the batched read has to recognise them
// itself, and a bucket that ever turns them on must not start reporting
// reaped keys as live ones on one transport and not the other.
func TestTheTwoTransportsAgreeAboutABrokerWrittenTombstone(t *testing.T) {
	nc := embeddedNATS(t)
	ctx := context.Background()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}

	bucket := newBucket(ctx, t, js, fmt.Sprintf("marked%d", bucketSeq.Add(1)),
		jetstream.KeyValueConfig{LimitMarkerTTL: time.Minute})
	if _, err := bucket.Create(ctx, "reaped", []byte(`{"n":1}`), jetstream.KeyTTL(behaviorTTL)); err != nil {
		t.Skipf("this broker does not support per-key TTL (%v); the broker-written "+
			"marker cannot be produced here, and skipping is not passing", err)
	}
	if _, err := bucket.Put(ctx, "kept", []byte(`{"n":2}`)); err != nil {
		t.Fatalf("seed the surviving key: %v", err)
	}
	waitReaped(ctx, t, bucket, "reaped")

	collect := func(name string, walk func(func(jetstream.KeyValueEntry) error) error) []string {
		t.Helper()
		var keys []string
		if err := walk(func(kve jetstream.KeyValueEntry) error {
			keys = append(keys, kve.Key())
			return nil
		}); err != nil {
			t.Fatalf("%s walk: %v", name, err)
		}
		slices.Sort(keys)
		return keys
	}

	ordered := collect("ordered", func(visit func(jetstream.KeyValueEntry) error) error {
		return watchWalk(ctx, bucket, visit)
	})
	batched := collect("batched", func(visit func(jetstream.KeyValueEntry) error) error {
		declined, err := directWalk(ctx, nc, bucket, directWalkMaxBytes, visit)
		if declined {
			t.Fatal("the embedded broker declined a batched read")
		}
		return err
	})

	if !slices.Equal(ordered, []string{"kept"}) {
		t.Fatalf("the ordered walk reported %v, want just the surviving key", ordered)
	}
	if !slices.Equal(ordered, batched) {
		t.Errorf("the two transports disagree about a broker-written tombstone:\n"+
			" ordered = %v\n batched = %v", ordered, batched)
	}
}
