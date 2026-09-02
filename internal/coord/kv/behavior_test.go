// Broker measurement harness — the empirical half of the coordination design.
//
// Every property the coordination contract rests on is a behaviour of
// JetStream KV, and none of them is in the client's documentation in a form
// you could design against. These are MEASUREMENTS, not unit tests:
// each one asserts the property the design depends on and prints the number the
// design is tuned to, so a broker upgrade that changes the behaviour fails here
// rather than in a seat handoff at 3am.
//
// Run them and read the numbers:
//
//	go test ./internal/coord/kv/ -run TestBrokerBehavior -v
//
// Measured on nats-server 2.14.5 / nats.go 1.53.1, embedded in-process with
// file storage:
//
//	bucket TTL (MaxAge) under test                1s
//	renewed through Update, held for              3x the bucket TTL, no lapse
//	unrenewed key reaped after                    1.306s — TTL + 306 ms
//	Get on a reaped key                           jetstream.ErrKeyNotFound
//	Create on the reaped key                      succeeds
//	epoch record after the lease key was reaped   still there, value intact
//	Create on an existing key                     jetstream.ErrKeyExists
//	Update at a stale revision                    jetstream.ErrKeyRevisionMismatch
//	per-key TTL (KeyTTL), never renewed           expires
//	per-key TTL (KeyTTL), renewed through Update  IMMORTAL — the trap
//	Get                                           40 µs
//	Update                                        40 µs
//	WatchAll over 21 keys                         720 µs
//
// Three of these decide something.
//
// The REAP LAG is not zero and is not bounded by the TTL: the broker sweeps
// expired messages on a timer, so a seat comes free at TTL + up to a few
// hundred milliseconds, not at TTL. Everything above this layer already treats
// takeover as eventual rather than instant, and this is the reason the number
// it is eventual by is the broker's and not the engine's.
//
// The SCAN COSTS 18 GETS at 20 seats and grows with the company, which is why
// an ungated claim — node presence, renewed on every heartbeat of every node —
// reads one key instead of listing the bucket.
//
// And PER-KEY TTL is why the leases bucket uses MaxAge. KeyTTL is create-only
// by design ("the TTL is set when the key is created and cannot be changed
// later") and Update clears it, so a lease renewed the obvious way never
// expires — and a dead node's seat could never be reclaimed. That failure is
// invisible in every functional test, because everything works; the seat just
// never comes back. The assertion below is here so a future maintainer cannot
// walk into it.
package kv

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// behaviorTTL is the bucket TTL these measurements run against. Short enough
// that waiting one out costs a second, long enough to be above nats-server's
// 100 ms floor on a stream MaxAge by an order of magnitude, so the reap lag
// being measured is the broker's own and not a rounding artefact.
const behaviorTTL = time.Second

// reapPoll is how often the harness asks whether an expired key is gone. It
// bounds the resolution of the reap-lag number, so it is well below the lag
// worth reporting.
const reapPoll = 5 * time.Millisecond

// results collects the numbers and prints them as one table, so a run reads
// like the summary above rather than like scattered log lines.
type results struct {
	mu   sync.Mutex
	rows [][2]string
}

func (r *results) record(what string, value any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, [2]string{what, fmt.Sprint(value)})
}

func (r *results) print(t *testing.T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	width := 0
	for _, row := range r.rows {
		if len(row[0]) > width {
			width = len(row[0])
		}
	}
	var b strings.Builder
	b.WriteString("\nJetStream KV behaviour, as this design depends on it\n")
	b.WriteString(strings.Repeat("=", width+34) + "\n")
	for _, row := range r.rows {
		fmt.Fprintf(&b, "%-*s  %s\n", width, row[0], row[1])
	}
	t.Log(b.String())
}

func TestBrokerBehavior(t *testing.T) {
	ctx := context.Background()
	nc := embeddedNATS(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream context: %v", err)
	}
	out := &results{}
	t.Cleanup(func() { out.print(t) })
	out.record("nats-server / nats.go", "2.14.5 / 1.53.1 (see go.mod)")
	out.record("bucket TTL (MaxAge) under test", behaviorTTL)

	leases := newBucket(ctx, t, js, "beh_leases", jetstream.KeyValueConfig{TTL: behaviorTTL})

	t.Run("create_on_an_existing_key_is_refused", func(t *testing.T) {
		// The exclusivity CAS. Without this, two nodes claiming one seat on
		// the same tick both believe they won.
		key := "excl"
		if _, err := leases.Create(ctx, key, []byte("first")); err != nil {
			t.Fatalf("first Create: %v", err)
		}
		_, err := leases.Create(ctx, key, []byte("second"))
		if !errors.Is(err, jetstream.ErrKeyExists) {
			t.Fatalf("Create on an existing key = %v, want jetstream.ErrKeyExists — "+
				"without this there is no mutual exclusion at all", err)
		}
		out.record("Create on an existing key", "jetstream.ErrKeyExists")
	})

	t.Run("update_at_a_stale_revision_is_refused", func(t *testing.T) {
		// The fencing CAS. Every renew, every takeover and every release is
		// predicated on it: a write from a tenure that has been superseded
		// must bounce rather than land.
		key := "fence"
		rev, err := leases.Create(ctx, key, []byte("v1"))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		newRev, err := leases.Update(ctx, key, []byte("v2"), rev)
		if err != nil {
			t.Fatalf("Update at the current revision: %v", err)
		}
		if newRev == rev {
			t.Fatalf("Update returned the same revision %d — a CAS needs a new one", rev)
		}
		_, err = leases.Update(ctx, key, []byte("v3"), rev)
		if !errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			t.Fatalf("Update at a stale revision = %v, want jetstream.ErrKeyRevisionMismatch — "+
				"a zombie's late write would land on the live tenure's record", err)
		}
		out.record("Update at a stale revision", "jetstream.ErrKeyRevisionMismatch")
	})

	t.Run("update_refreshes_the_entrys_age", func(t *testing.T) {
		// THE load-bearing measurement. The bucket's MaxAge is the renewable
		// TTL: because every write restarts an entry's age, a heartbeat that
		// Updates is a renew, with no second mechanism and nothing to keep
		// in sync.
		key := "renewed"
		rev, err := leases.Create(ctx, key, []byte("held"))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		const renewals = 6
		start := time.Now()
		for i := range renewals {
			time.Sleep(behaviorTTL / 2)
			rev, err = leases.Update(ctx, key, []byte("held"), rev)
			if err != nil {
				t.Fatalf("renew %d at %v: %v — Update does not refresh the entry's age, so a "+
					"held lease would lapse under its own owner", i, time.Since(start), err)
			}
		}
		held := time.Since(start)
		if held < 2*behaviorTTL {
			t.Fatalf("only held the key for %v; the measurement needs to outlast the bucket TTL", held)
		}
		out.record("renewed through Update, held for", fmt.Sprintf("%v (%.1fx the bucket TTL)",
			held.Round(10*time.Millisecond), held.Seconds()/behaviorTTL.Seconds()))
	})

	t.Run("an_unrenewed_key_expires_server_side", func(t *testing.T) {
		// The other half: a node that dies stops writing, and the broker —
		// not a peer, and not anybody's wall clock — decides the seat is
		// free. This is the arbiter Postgres now() used to be.
		key := "abandoned"
		if _, err := leases.Create(ctx, key, []byte("held")); err != nil {
			t.Fatalf("Create: %v", err)
		}
		written := time.Now()
		deadline := written.Add(behaviorTTL + 10*time.Second)
		var gone time.Duration
		for {
			if time.Now().After(deadline) {
				t.Fatalf("the key was still readable %v after its last write — an unrenewed "+
					"lease that never expires means a dead node's seat is never reclaimed",
					time.Since(written))
			}
			_, err := leases.Get(ctx, key)
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				gone = time.Since(written)
				break
			}
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			time.Sleep(reapPoll)
		}
		if gone < behaviorTTL {
			t.Fatalf("the key vanished after %v, sooner than its %v TTL — a lease would end "+
				"before the deadline its holder was handed", gone, behaviorTTL)
		}
		out.record("unrenewed key reaped after", fmt.Sprintf("%v (TTL + %v of broker lag)",
			gone.Round(time.Millisecond), (gone-behaviorTTL).Round(time.Millisecond)))
		out.record("Get on a reaped key", "jetstream.ErrKeyNotFound")

		// And the successor's claim lands on the key the broker removed.
		if _, err := leases.Create(ctx, key, []byte("successor")); err != nil {
			t.Fatalf("Create after the key expired: %v — the seat could not be taken over", err)
		}
		out.record("Create on the reaped key", "succeeds")
	})

	t.Run("a_bucket_without_a_ttl_outlives_one_with_it", func(t *testing.T) {
		// Why the epoch needs its own bucket. If the counter lived on the
		// lease key it would be DELETED with it, and the next owner would be
		// handed epoch 1 — the token a zombie from the previous tenure is
		// still stamping its writes with.
		epochs := newBucket(ctx, t, js, "beh_epochs", jetstream.KeyValueConfig{})
		if _, err := epochs.Create(ctx, "counter", []byte(`{"epoch":7}`)); err != nil {
			t.Fatalf("Create in the untimed bucket: %v", err)
		}
		key := "paired"
		if _, err := leases.Create(ctx, key, []byte("held")); err != nil {
			t.Fatalf("Create in the timed bucket: %v", err)
		}
		waitReaped(ctx, t, leases, key)

		kve, err := epochs.Get(ctx, "counter")
		if err != nil {
			t.Fatalf("the counter went with the lease key: %v — this is the whole reason "+
				"there are two buckets", err)
		}
		if string(kve.Value()) != `{"epoch":7}` {
			t.Fatalf("counter reads %s, want the value it was written with", kve.Value())
		}
		out.record("epoch record after the lease key was reaped", "still there, value intact")
	})

	t.Run("per_key_ttl_is_not_renewable", func(t *testing.T) {
		// THE TRAP. jetstream.KeyTTL looks like exactly the right primitive
		// and is the wrong one: it is create-only by design, and Update
		// clears it. A lease renewed through Update becomes IMMORTAL, which
		// means a dead node's seat can never be reclaimed — and nothing
		// fails, so nothing tells you.
		//
		// Both halves are asserted. The first proves KeyTTL works at all, so
		// the second cannot be explained away as a broken fixture.
		marked := newBucket(ctx, t, js, "beh_perkey", jetstream.KeyValueConfig{
			LimitMarkerTTL: time.Minute,
		})

		if _, err := marked.Create(ctx, "untouched", []byte("v"), jetstream.KeyTTL(behaviorTTL)); err != nil {
			t.Skipf("this broker does not support per-key TTL (%v); the trap cannot be "+
				"measured here, and skipping is not passing", err)
		}
		waitReaped(ctx, t, marked, "untouched")
		out.record("per-key TTL (KeyTTL), never renewed", "expires")

		rev, err := marked.Create(ctx, "renewed", []byte("v"), jetstream.KeyTTL(behaviorTTL))
		if err != nil {
			t.Fatalf("Create with a per-key TTL: %v", err)
		}
		// One renew, well inside the TTL — exactly what a heartbeat does.
		time.Sleep(behaviorTTL / 3)
		if _, err := marked.Update(ctx, "renewed", []byte("v"), rev); err != nil {
			t.Fatalf("Update: %v", err)
		}
		// Wait out three whole TTLs with no further writes. A renewable TTL
		// would have reaped this twice over.
		time.Sleep(3 * behaviorTTL)
		_, err = marked.Get(ctx, "renewed")
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			t.Fatalf("a key created with a %v per-key TTL and renewed once through Update was "+
				"reaped after %v of silence. That is the OPPOSITE of what was measured when "+
				"this design was chosen. If per-key TTL has become renewable, the leases "+
				"bucket could use it — but re-measure before believing it",
				behaviorTTL, 3*behaviorTTL)
		}
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		out.record("per-key TTL (KeyTTL), renewed through Update",
			fmt.Sprintf("IMMORTAL — alive %v after the last write", 3*behaviorTTL))
	})

	t.Run("round_trip_costs", func(t *testing.T) {
		// Not a property, a budget. Every one of these is on the seat
		// heartbeat's path, and the claim path pays several — which is what
		// makes a scan-per-renew the thing to avoid rather than a detail.
		bucket := newBucket(ctx, t, js, "beh_cost", jetstream.KeyValueConfig{TTL: time.Minute})
		const samples = 40
		for i := range 20 {
			if _, err := bucket.Create(ctx, fmt.Sprintf("seat=3Aagent-%02d", i), []byte(`{"owner":"n"}`)); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		rev, err := bucket.Create(ctx, "probe", []byte(`{"owner":"n"}`))
		if err != nil {
			t.Fatalf("Create probe: %v", err)
		}

		out.record("Get, median of 40", median(t, samples, func() error {
			_, err := bucket.Get(ctx, "probe")
			return err
		}))
		out.record("Update, median of 40", median(t, samples, func() error {
			next, err := bucket.Update(ctx, "probe", []byte(`{"owner":"n"}`), rev)
			if err == nil {
				rev = next
			}
			return err
		}))
		out.record("WatchAll over 21 keys, median of 40", median(t, samples, func() error {
			w, err := bucket.WatchAll(ctx, jetstream.IgnoreDeletes())
			if err != nil {
				return err
			}
			defer func() { _ = w.Stop() }()
			for kve := range w.Updates() {
				if kve == nil {
					return nil
				}
			}
			return errors.New("watcher closed before the initial values ended")
		}))
	})
}

func newBucket(ctx context.Context, t *testing.T, js jetstream.JetStream, name string, cfg jetstream.KeyValueConfig) jetstream.KeyValue {
	t.Helper()
	cfg.Bucket = name
	kv, err := js.CreateOrUpdateKeyValue(ctx, cfg)
	if err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
	return kv
}

// waitReaped blocks until the broker has removed a key, failing the test if it
// never does.
func waitReaped(ctx context.Context, t *testing.T, kv jetstream.KeyValue, key string) {
	t.Helper()
	deadline := time.Now().Add(behaviorTTL + 10*time.Second)
	for time.Now().Before(deadline) {
		_, err := kv.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return
		}
		if err != nil {
			t.Fatalf("Get %s: %v", key, err)
		}
		time.Sleep(reapPoll)
	}
	t.Fatalf("%s/%s was never reaped", kv.Bucket(), key)
}

// median times n calls and reports the middle one. The median rather than the
// mean because one scheduling hiccup in a test container should not become the
// number a design is budgeted against.
func median(t *testing.T, n int, call func() error) time.Duration {
	t.Helper()
	taken := make([]time.Duration, 0, n)
	for range n {
		start := time.Now()
		if err := call(); err != nil {
			t.Fatalf("timed call failed: %v", err)
		}
		taken = append(taken, time.Since(start))
	}
	slices.SortFunc(taken, func(a, b time.Duration) int { return cmp.Compare(a, b) })
	return taken[len(taken)/2].Round(10 * time.Microsecond)
}
