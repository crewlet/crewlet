package kv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// embeddedNATS starts a nats-server inside the test process with no listener,
// the same topology internal/queue/jetstream boots for a solo node. Nothing
// outside this process can reach it, and it dies with the test.
func embeddedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	dir := t.TempDir()
	ns, err := server.NewServer(&server.Options{
		ServerName: "coordkv-test",
		JetStream:  true,
		Port:       -1,
		DontListen: true,
		StoreDir:   dir,
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

// bucketSeq gives every store its own pair of buckets.
var bucketSeq atomic.Int64

func openStore(t *testing.T, nc *nats.Conn, ttl time.Duration) *Store {
	t.Helper()
	prefix := fmt.Sprintf("t%d", bucketSeq.Add(1))
	s, err := Open(context.Background(), nc, Config{TTL: ttl, BucketPrefix: prefix})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// TestContract runs the one suite every coord.Backend is certified by.
//
// Two things about how it is wired here.
//
// The store's TTL is coordtest.LongTTL because the bucket's MaxAge has to be
// able to accommodate the longest TTL any case asks for — a shorter bucket
// would reap a LongTTL record early, and this backend refuses a TTL it cannot
// honour rather than quietly shortening it. The suite's ShortTTL and churnTTL
// leases are honoured by the deadline the record carries, judged against the
// STORE's clock (Store.resolveNow), never the test process's.
//
// And there is no coordtest.Advancer: a real broker's clock is its own and
// cannot be moved, which is exactly the case the hook was made optional for.
// So harness.lapse falls back to a real sleep of ShortTTL + margin — 150 ms
// per lapsing case, overlapped because the suite runs its cases in parallel.
func TestContract(t *testing.T) {
	nc := embeddedNATS(t)
	coordtest.Run(t, func(t *testing.T) coord.Backend {
		return openStore(t, nc, coordtest.LongTTL)
	})
}

func TestKeyMappingRoundTrips(t *testing.T) {
	t.Parallel()
	// A resource containing a dot is the one that matters most: a dot is a
	// SUBJECT SEPARATOR in a NATS key, so an unescaped one would silently
	// split the key into two tokens.
	resources := []string{
		"seat:alice",
		"seat:alice.smith",
		"node:node-0:aaaa",
		"worker:scheduler",
		"seat:a=3Ab",
		"seat:a/b",
		"seat:a_b-c",
		"seat:ünïcødé",
		"seat: leading space",
		"plain",
		"=",
		".",
		":",
	}
	seen := map[string]string{}
	for _, r := range resources {
		key := encodeKey(r)
		if !validKeyForNATS(key) {
			t.Fatalf("encodeKey(%q) = %q, which NATS KV will not accept", r, key)
		}
		back, ok := decodeKey(key)
		if !ok {
			t.Fatalf("decodeKey(%q) (from %q) reported an unreadable key", key, r)
		}
		if back != r {
			t.Fatalf("round trip: %q -> %q -> %q", r, key, back)
		}
		if other, clash := seen[key]; clash {
			t.Fatalf("resources %q and %q both encode to %q — one seat, two names", other, r, key)
		}
		seen[key] = r
	}
}

// validKeyForNATS mirrors nats.go's own key rule (jetstream/kv.go validKeyRe).
func validKeyForNATS(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '/', c == '_', c == '=', c == '.':
		default:
			return false
		}
	}
	return true
}

func TestDecodeKeyRejectsWhatItDidNotWrite(t *testing.T) {
	t.Parallel()
	bad := []string{
		"",
		"=",           // truncated escape
		"=3",          // truncated escape
		"=zz",         // not hex
		"=3a",         // lower-case hex: encodeKey only emits upper, and
		"a.b",         // accepting both would collide two keys on one resource
		"a/b",         // '/' is legal in a key but this encoder never emits one
		"seat\x00bad", // not a legal key at all
	}
	for _, key := range bad {
		if got, ok := decodeKey(key); ok {
			t.Fatalf("decodeKey(%q) = (%q, true), want a rejection", key, got)
		}
	}
}

// TestAwkwardResourceNamesSurviveTheStore takes the mapping end to end. The
// round-trip test above proves encodeKey/decodeKey are inverses; this proves
// nothing between them — the listings, the prefix filters, the two buckets —
// reads a resource by anything but its real name.
func TestAwkwardResourceNamesSurviveTheStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	s := openStore(t, nc, time.Minute)

	// A dot is a SUBJECT SEPARATOR in a NATS key, so an unescaped one would
	// split the key into two tokens; a handle really can contain one.
	dotted := coord.SeatResource("alice.smith")
	plain := coord.SeatResource("alice")
	for _, r := range []string{dotted, plain} {
		if _, err := s.TryAcquire(ctx, r, coord.AcquireOptions{
			Owner: "node-a:1", TTL: time.Minute, Preferred: "node-a",
		}); err != nil {
			t.Fatalf("claim %q: %v", r, err)
		}
	}
	if lease := mustGet(t, s, dotted); lease.Resource != dotted {
		t.Fatalf("Get(%q) answered about %q", dotted, lease.Resource)
	}
	owned, err := s.ListOwned(ctx, "node-a:1")
	if err != nil {
		t.Fatalf("ListOwned: %v", err)
	}
	if len(owned) != 2 {
		t.Fatalf("ListOwned = %v, want both seats", owned)
	}
	live, err := s.ListLive(ctx, coord.SeatPrefix)
	if err != nil {
		t.Fatalf("ListLive: %v", err)
	}
	if len(live) != 2 {
		t.Fatalf("ListLive(%q) = %v, want both seats — the prefix is matched on the "+
			"RESOURCE, not on the escaped key", coord.SeatPrefix, live)
	}
	hints, err := s.PreferredResources(ctx, coord.SeatPrefix, "node-a")
	if err != nil {
		t.Fatalf("PreferredResources: %v", err)
	}
	if _, ok := hints[dotted]; !ok || len(hints) != 2 {
		t.Fatalf("hints = %v, want both seats including %q", hints, dotted)
	}
}

func mustGet(t *testing.T, s *Store, resource string) *coord.Lease {
	t.Helper()
	lease, err := s.Get(context.Background(), resource)
	if err != nil {
		t.Fatalf("Get(%q): %v", resource, err)
	}
	if lease == nil {
		t.Fatalf("Get(%q): nothing holds it", resource)
	}
	return lease
}

func TestConfigIsValidated(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no TTL", Config{}},
		{"TTL below the broker's floor", Config{TTL: 50 * time.Millisecond}},
		{"negative TTL", Config{TTL: -time.Second}},
		{"bucket prefix with a dot", Config{TTL: time.Minute, BucketPrefix: "a.b"}},
		{"bucket prefix with a colon", Config{TTL: time.Minute, BucketPrefix: "a:b"}},
		{"too many replicas", Config{TTL: time.Minute, Replicas: 9}},
	}
	for _, c := range cases {
		if _, err := Open(context.Background(), nc, c.cfg); err == nil {
			t.Fatalf("Open with %s was accepted", c.name)
		}
	}
	if _, err := Open(context.Background(), nil, Config{TTL: time.Minute}); err == nil {
		t.Fatal("Open with no connection was accepted")
	}
}

func TestATTLLongerThanTheBucketIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	s := openStore(t, nc, time.Minute)

	// The refusal is an ERROR, not a (nil, nil) refusal: nobody else holds
	// the seat, the caller asked for something the bucket cannot promise.
	lease, err := s.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: 2 * time.Minute})
	if err == nil {
		t.Fatalf("TryAcquire with a TTL above the bucket's = (%v, nil), want an error", lease)
	}
	if lease != nil {
		t.Fatal("TryAcquire granted a lease it cannot keep alive that long")
	}
	if !errors.Is(err, errTTLTooLong) {
		t.Fatalf("error %v does not name the TTL mismatch", err)
	}
	if _, err := s.Renew(ctx, "seat:ceo", "node-a", 1, 2*time.Minute); !errors.Is(err, errTTLTooLong) {
		t.Fatalf("Renew with a TTL above the bucket's = %v", err)
	}
	// Exactly the configured TTL is the normal case and must be accepted.
	if _, err := s.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: time.Minute}); err != nil {
		t.Fatalf("TryAcquire at exactly the bucket TTL: %v", err)
	}
}

// TestServerSideExpiryHandsTheSeatOver is the production shape the contract
// suite cannot reach: every lease at the configured TTL, so the record's own
// disappearance is the expiry and no clock is consulted at all.
func TestServerSideExpiryHandsTheSeatOver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	const ttl = time.Second
	s := openStore(t, nc, ttl)

	first, err := s.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{
		Owner: "node-a:1", TTL: ttl, Preferred: "node-a",
	})
	if err != nil || first == nil {
		t.Fatalf("first claim = (%v, %v)", first, err)
	}

	// A renew through Update refreshes the record's age, which is the only
	// reason a lease can be held for longer than the bucket's TTL.
	for range 3 {
		time.Sleep(ttl / 2)
		ok, err := s.Renew(ctx, "seat:ceo", "node-a:1", first.Epoch, ttl)
		if err != nil || !ok {
			t.Fatalf("renew at %v = (%v, %v) — Update is not refreshing the entry's age", ttl/2, ok, err)
		}
	}
	if got, err := s.Get(ctx, "seat:ceo"); err != nil || got == nil {
		t.Fatalf("after renewing past the bucket TTL: (%v, %v)", got, err)
	}

	// The node dies. Nothing renews; the broker reaps the key.
	time.Sleep(ttl + 500*time.Millisecond)
	if got, err := s.Get(ctx, "seat:ceo"); err != nil || got != nil {
		t.Fatalf("an unrenewed lease is still readable: (%v, %v)", got, err)
	}
	if raw, err := s.leases.Get(ctx, encodeKey("seat:ceo")); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatalf("the lease KEY survived its bucket TTL: (%v, %v)", raw, err)
	}

	// The peer's claim lands through Create, on a key the broker removed.
	taken, err := s.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{Owner: "node-b:1", TTL: ttl})
	if err != nil || taken == nil {
		t.Fatalf("takeover after a server-side expiry = (%v, %v)", taken, err)
	}
	// The whole reason the epoch lives in its own bucket: the lease key was
	// deleted by the broker and the counter still went up.
	if taken.Epoch <= first.Epoch {
		t.Fatalf("epoch %d after the lease key expired at epoch %d — the counter reset with the key",
			taken.Epoch, first.Epoch)
	}
	// And the hint survived with it, which a lease-bucket-only hint could
	// not have done: the key it lived on no longer exists.
	if taken.Preferred != "node-a" {
		t.Fatalf("placement hint is %q after the lease key expired, want node-a", taken.Preferred)
	}
	hints, err := s.PreferredResources(ctx, coord.SeatPrefix, "node-a")
	if err != nil {
		t.Fatalf("PreferredResources: %v", err)
	}
	if _, ok := hints["seat:ceo"]; !ok {
		t.Fatalf("hints for node-a = %v after the lease key expired, want seat:ceo", hints)
	}
}

func TestReleaseExpiresInPlaceAndKeepsTheKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	s := openStore(t, nc, time.Minute)

	lease, err := s.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: time.Minute})
	if err != nil || lease == nil {
		t.Fatalf("claim = (%v, %v)", lease, err)
	}
	if ok, err := s.Release(ctx, "seat:ceo", "node-a", lease.Epoch); err != nil || !ok {
		t.Fatalf("release = (%v, %v)", ok, err)
	}
	// A delete would take the record away; a tombstone leaves it, which is
	// what keeps the resource's history readable while it is unheld.
	kve, err := s.leases.Get(ctx, encodeKey("seat:ceo"))
	if err != nil {
		t.Fatalf("the released key was deleted, not expired in place: %v", err)
	}
	if op := kve.Operation(); op != jetstream.KeyValuePut {
		t.Fatalf("released key carries operation %v, want a plain Put (a tombstone VALUE, not a KV delete)", op)
	}
	if !strings.Contains(string(kve.Value()), `"owner":""`) {
		t.Fatalf("released record %s does not read as unheld", kve.Value())
	}
}

func TestUngatedClaimsDoNotScanTheFleet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	s := openStore(t, nc, time.Minute)

	// An older peer holds a seat. A presence registration must still land —
	// membership is not work, and a newer node invisible in the membership
	// read makes every peer divide the seats by a fleet that excludes it.
	if _, err := s.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{
		Owner: "old:1", TTL: time.Minute, Protocol: 1,
	}); err != nil {
		t.Fatalf("old claim: %v", err)
	}
	presence, err := s.TryAcquire(ctx, coord.NodeResource("new"), coord.AcquireOptions{
		Owner: "new:1", TTL: time.Minute, Protocol: coord.ProtocolVersion, Ungated: true,
		Meta: map[string]any{"roles": []any{"seats"}},
	})
	if err != nil || presence == nil {
		t.Fatalf("ungated presence claim = (%v, %v)", presence, err)
	}
	if presence.Protocol != coord.ProtocolVersion {
		t.Fatalf("ungated claim recorded protocol %d", presence.Protocol)
	}
	// And its meta round-tripped through JSON.
	if got := presence.Meta["roles"]; fmt.Sprint(got) != "[seats]" {
		t.Fatalf("meta roles = %v", got)
	}
}

// TestFleetContract runs the shared-state suite against the real broker.
//
// The retentions are the suite's, not production's: every case reasons about
// a window or a claim it names both sides of, and a bucket sized for the
// production ledger's seven days would make a "this has lapsed" case wait
// seven days. What the broker is being certified for is the SEMANTICS — an
// atomic increment, a Create that only one caller wins, a Put whose revision
// is the epoch — and those do not vary with the retention.
func TestFleetContract(t *testing.T) {
	nc := embeddedNATS(t)
	coordtest.RunFleet(t, func(t *testing.T) coord.Fleet {
		prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
		store, err := OpenFleet(context.Background(), nc, FleetConfig{
			BucketPrefix: prefix,
			// Every one of these is above the broker's 100 ms floor and
			// far longer than a case takes, so nothing lapses under a
			// case that did not ask it to.
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
	})
}

// A CORRUPT CREDENTIAL RECORD RAISES RATHER THAN VANISHING.
//
// The contract suite cannot plant one — PutSecret validates on the way in —
// but the decode path is where a bad record would be swallowed, and swallowing
// is the failure that matters here. SecretValues IS the engine's boot
// snapshot: a name silently dropped from it resolves as an empty ${VAR} on
// every node at once, which reaches a provider as an empty credential and
// comes back as an auth failure blamed on the vendor. "I could not read it"
// has to be louder than "there is none".
func TestAnUndecodableSecretIsRaisedNotSkipped(t *testing.T) {
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
	store, err := OpenFleet(context.Background(), nc, FleetConfig{
		RateWindow: time.Minute, ClaimTTL: time.Minute,
		LedgerRetention: time.Minute, FireRetention: time.Minute,
		CooldownMax: time.Minute, StatusFreshness: time.Minute,
		BucketPrefix: prefix,
	})
	if err != nil {
		t.Fatalf("OpenFleet: %v", err)
	}
	ctx := t.Context()
	if err := store.PutSecret(ctx, coord.SecretRecord{
		Name: "GOOD", Value: "v1:sealed", KeyID: "key-1", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	// Straight into the bucket, past the contract's validation — an
	// operator's stray write, a half-finished migration, a truncated value.
	if _, err := store.secrets.Put(ctx, encodeKey("BROKEN"), []byte("{not json")); err != nil {
		t.Fatalf("planting the corrupt record: %v", err)
	}

	if _, _, err := store.Secret(ctx, "BROKEN"); err == nil {
		t.Error("a corrupt record read back as a credential")
	}
	rows, err := store.SecretValues(ctx)
	if err == nil {
		t.Fatalf("the boot snapshot skipped the corrupt record and returned %d "+
			"rows — every ${VAR} it should have carried is now empty", len(rows))
	}
	// And the healthy one is still readable, so the failure is the record's
	// rather than the bucket's.
	if _, found, err := store.Secret(ctx, "GOOD"); err != nil || !found {
		t.Fatalf("GOOD: found=%v err=%v", found, err)
	}
}
