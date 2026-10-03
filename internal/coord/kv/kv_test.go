package kv

import (
	"context"
	"encoding/json"
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
	"github.com/crewlet/crewlet/internal/period"
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

// bucketSeq gives every store its own set of buckets.
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
// STORE's clock (Store.storeNow), never the test process's. The duty cases
// are the exception the suite names: they claim at coord.MaxDutyTTL, far past
// this bucket's age, and are honoured by the duty bucket, whose ceiling is the
// contract's rather than this configuration's.
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
		"bulk:a project:7",
	}
	seen := map[string]string{}
	for _, r := range resources {
		key := encodeResource(r)
		if !validKeyForNATS(key) {
			t.Fatalf("encodeResource(%q) = %q, which NATS KV will not accept", r, key)
		}
		back, ok := decodeResource(key)
		if !ok {
			t.Fatalf("decodeResource(%q) (from %q) reported an unreadable key", key, r)
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
	// split the key into two tokens.
	//
	// BUILT FROM THE CLASS DIRECTLY rather than through coord.SeatResource,
	// which takes a uuid and can no longer produce a dotted name. What is
	// under test is the STORE's key encoding over an arbitrary resource
	// name, and there is nothing in the contract that says a class's names
	// are uuid-shaped — a company name, a duty and a stream all reach these
	// buckets, and each can carry a dot.
	dotted := coord.ClassSeat.Resource("alice.smith")
	plain := coord.ClassSeat.Resource("alice")
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
	live, err := s.ListLive(ctx, coord.ClassSeat)
	if err != nil {
		t.Fatalf("ListLive: %v", err)
	}
	if len(live) != 2 {
		t.Fatalf("ListLive(%q) = %v, want both seats — the prefix is matched on the "+
			"RESOURCE, not on the escaped key", coord.ClassSeat, live)
	}
	hints, err := s.PreferredResources(ctx, coord.ClassSeat, "node-a")
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
	if raw, err := s.leases.kv.Get(ctx, encodeResource("seat:ceo")); !errors.Is(err, jetstream.ErrKeyNotFound) {
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
	hints, err := s.PreferredResources(ctx, coord.ClassSeat, "node-a")
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
	kve, err := s.leases.kv.Get(ctx, encodeResource("seat:ceo"))
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
	coordtest.RunFleet(t, func(t *testing.T, ages coordtest.FleetAges) coord.Fleet {
		prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
		store, err := OpenFleet(context.Background(), nc, FleetConfig{
			BucketPrefix: prefix,
			// THE AGES THE CASE ASKED FOR go to the buckets the suite
			// reasons about; the rest are above the broker's 100 ms
			// floor and far longer than a case takes, so nothing lapses
			// under a case that did not ask it to.
			ClaimTTL:           ages.Claim,
			SetupOnceRetention: ages.Setup,
			RateWindow:         time.Minute,
			LedgerRetention:    10 * time.Minute,
			FireRetention:      10 * time.Minute,
			FollowRetention:    10 * time.Minute,
			CooldownMax:        time.Hour,
			BudgetRetention:    time.Hour,
			StatusFreshness:    10 * time.Minute,
		})
		if err != nil {
			t.Fatalf("OpenFleet: %v", err)
		}
		return store
	})
}

// payloadFleet opens a fleet store whose config bucket a case can reach.
func payloadFleet(t *testing.T) *FleetStore {
	t.Helper()
	store, err := OpenFleet(t.Context(), embeddedNATS(t), FleetConfig{
		RateWindow: time.Minute, ClaimTTL: time.Minute, SetupOnceRetention: time.Minute,
		LedgerRetention: time.Minute, FireRetention: time.Minute,
		FollowRetention: time.Minute,
		CooldownMax:     time.Minute, StatusFreshness: time.Minute,
		BudgetRetention: time.Minute,
		BucketPrefix:    fmt.Sprintf("f%d", bucketSeq.Add(1)),
	})
	if err != nil {
		t.Fatalf("OpenFleet: %v", err)
	}
	return store
}

// THE BODY RIDES THE POINTER AS THE JSON IT IS — and is mirrored beside it.
//
// It was bytes on a key of its own, which encoding/json spells as a base64
// string — over an envelope whose ciphertext is base64 already, so every body
// crossed the wire encoded twice and a third larger than it was. The contract
// suite cannot see the record, only what Payload hands back, so the WIRE SHAPE
// is asserted here: the pointer's own record carries the body as an object,
// and the key an earlier build reads it from carries the same body, naming the
// pointer's revision and the epoch it copies.
//
// Mutation: carry the payload as []byte in activationRecord and the field is a
// string; stop mirroring and the legacy key is absent.
func TestThePointerCarriesItsBodyAsAnObject(t *testing.T) {
	t.Parallel()
	store := payloadFleet(t)
	body := `{"__encrypted__":"enc:v1:k1:q8+/Zm9vYmFy=="}`
	activation, err := store.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-1", Payload: []byte(body), At: time.Now()})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	entry, err := store.config.Get(t.Context(), activationKey)
	if err != nil {
		t.Fatalf("read the pointer: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(entry.Value(), &fields); err != nil {
		t.Fatalf("decode the pointer: %v", err)
	}
	if got := string(fields["payload"]); got != body {
		t.Errorf("the pointer's payload field is %s, want the object %s", got, body)
	}
	mirror, err := store.config.Get(t.Context(), legacyPayloadKey)
	if err != nil {
		t.Fatalf("no body was mirrored beside the pointer: %v", err)
	}
	var record legacyPayloadRecord
	if err := json.Unmarshal(mirror.Value(), &record); err != nil {
		t.Fatalf("decode the mirror: %v", err)
	}
	if record.RevisionID != "rev-1" || string(record.Payload) != body ||
		int64(record.Epoch) != activation.Epoch {
		t.Errorf("mirror = {%s %s epoch %d}, want {rev-1 %s epoch %d}",
			record.RevisionID, record.Payload, record.Epoch, body, activation.Epoch)
	}
}

// earlierBuildRead converges the way a build from before the body moved into
// the pointer does, and it is the whole of that build's read: the pointer in
// ITS shape, which has no payload field, and then the body beside it, taken
// only when it names the revision the pointer does. What it returns is what
// such a node would apply, and ok is false where it would record "no such
// revision".
func earlierBuildRead(t *testing.T, store *FleetStore) (revision string, body []byte, ok bool) {
	t.Helper()
	ctx := t.Context()
	entry, err := store.config.Get(ctx, activationKey)
	if err != nil {
		t.Fatalf("read the pointer: %v", err)
	}
	var pointer struct {
		RevisionID string `json:"revision_id"`
	}
	if err := json.Unmarshal(entry.Value(), &pointer); err != nil {
		t.Fatalf("decode the pointer in the earlier shape: %v", err)
	}
	beside, err := store.config.Get(ctx, legacyPayloadKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return pointer.RevisionID, nil, false
	}
	if err != nil {
		t.Fatalf("read the body beside the pointer: %v", err)
	}
	var record struct {
		RevisionID string `json:"revision_id"`
		Payload    []byte `json:"payload"`
	}
	if err := json.Unmarshal(beside.Value(), &record); err != nil {
		t.Fatalf("decode the body in the earlier shape: %v", err)
	}
	return pointer.RevisionID, record.Payload, record.RevisionID == pointer.RevisionID
}

// A PEER ON AN EARLIER BUILD CONVERGES ON EVERY ACTIVATION THIS BUILD MAKES.
//
// A rolling upgrade puts both builds on one bucket, and the fleet activates
// during a rollout with nobody editing — the integration loop re-activates the
// revision when it seals a credential, and a node booting ahead of the pointer
// publishes its own. The earlier build reads the body only from the key
// beside the pointer, so a build that stopped writing it left every such peer
// failing the apply, shedding its work, and three attempts later unready.
//
// The lost compare-and-set is the case the mirror's ORDER is for: the earlier
// build wrote the body before its flip, so a write whose flip was refused had
// already replaced the body the fleet's pointer named.
//
// Mutation: drop the mirror and the first read finds nothing; write it before
// the flip and the refused write leaves its own body there.
func TestAPeerOnAnEarlierBuildReadsEveryActivation(t *testing.T) {
	t.Parallel()
	store := payloadFleet(t)
	ctx := t.Context()
	activate := func(req coord.ActivationRequest) error {
		t.Helper()
		req.At = time.Now()
		_, err := store.Activate(ctx, req)
		return err
	}
	expect := func(revision, body string) {
		t.Helper()
		got, payload, ok := earlierBuildRead(t, store)
		if !ok || got != revision || string(payload) != body {
			t.Fatalf("an earlier build reads %s = %s (ok=%v), want %s = %s",
				got, payload, ok, revision, body)
		}
	}

	if err := activate(coord.ActivationRequest{
		RevisionID: "rev-1", Payload: []byte(`{"v":1}`)}); err != nil {
		t.Fatalf("Activate rev-1: %v", err)
	}
	expect("rev-1", `{"v":1}`)

	if err := activate(coord.ActivationRequest{
		RevisionID: "rev-2", Payload: []byte(`{"v":2}`), Expect: "rev-1"}); err != nil {
		t.Fatalf("Activate rev-2: %v", err)
	}
	expect("rev-2", `{"v":2}`)

	// THE REFUSED WRITE: built on rev-1, which is no longer current.
	if err := activate(coord.ActivationRequest{
		RevisionID: "rev-lost", Payload: []byte(`{"v":"lost"}`), Expect: "rev-1",
	}); !errors.Is(err, coord.ErrActivationRaced) {
		t.Fatalf("a write built on a superseded revision = %v, want the race", err)
	}
	expect("rev-2", `{"v":2}`)

	// And the credential-rotation gesture, an unchanged revision again.
	if err := activate(coord.ActivationRequest{
		RevisionID: "rev-2", Payload: []byte(`{"v":2}`)}); err != nil {
		t.Fatalf("re-activate rev-2: %v", err)
	}
	expect("rev-2", `{"v":2}`)
}

// RACING ACTIVATIONS LEAVE THE MIRROR NAMING THE POINTER.
//
// Two nodes booting together each publish what they hold, so activations race
// in ordinary operation. A mirror that was a plain write after each flip would
// end on whichever write landed last, which need not be the flip that did —
// and every peer on the earlier build would read "no such revision" for the
// epoch the fleet is on until somebody activated again.
func TestRacingActivationsLeaveTheMirrorNamingThePointer(t *testing.T) {
	t.Parallel()
	for round := range 4 {
		store := payloadFleet(t)
		start := make(chan struct{})
		errs := make(chan error, 8)
		for i := range 8 {
			go func() {
				<-start
				_, err := store.Activate(t.Context(), coord.ActivationRequest{
					RevisionID: fmt.Sprintf("rev-%d", i),
					Payload:    []byte(fmt.Sprintf(`{"v":%d}`, i)), At: time.Now(),
				})
				errs <- err
			}()
		}
		close(start)
		for range 8 {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: Activate: %v", round, err)
			}
		}
		target, found, err := store.Target(t.Context())
		if err != nil || !found {
			t.Fatalf("round %d: Target = found %v, err %v", round, found, err)
		}
		revision, body, ok := earlierBuildRead(t, store)
		want := fmt.Sprintf(`{"v":%s}`, strings.TrimPrefix(target.RevisionID, "rev-"))
		if !ok || revision != target.RevisionID || string(body) != want {
			t.Fatalf("round %d: after racing activations an earlier build reads "+
				"%s = %s (ok=%v), and the fleet is on %s", round, revision, body, ok,
				target.RevisionID)
		}
	}
}

// A MIRROR NEVER STEPS BACK, AND NEVER UNNAMES A LATER POINTER'S BODY.
//
// Two fences, each deterministic here because the case writes the state a
// race would leave. A writer slow to mirror finds a mirror a LATER epoch
// already wrote, and must leave it: the body it holds is not the one the
// fleet is on. And a writer of this build finds the pointer moved on by an
// EARLIER build, whose pointer carries no body and whose body sits in the
// mirror's key: replacing it leaves a pointer no node of either build can
// read.
//
// Mutation: drop the epoch comparison and the first half replaces the later
// mirror; drop the pointer comparison and the second half replaces the
// earlier build's body.
func TestAMirrorNeverStepsBack(t *testing.T) {
	t.Parallel()
	// EACH SUBTEST'S OWN t AND CONTEXT: a helper failing through the
	// parent's t from a subtest's goroutine is a FailNow on the wrong test.
	put := func(t *testing.T, store *FleetStore, key string, v any) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.config.Put(t.Context(), key, raw); err != nil {
			t.Fatalf("plant %s: %v", key, err)
		}
	}
	held := func(t *testing.T, store *FleetStore) legacyPayloadRecord {
		t.Helper()
		entry, err := store.config.Get(t.Context(), legacyPayloadKey)
		if err != nil {
			t.Fatalf("read the mirror: %v", err)
		}
		var record legacyPayloadRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			t.Fatalf("decode the mirror: %v", err)
		}
		return record
	}

	t.Run("a later epoch's mirror", func(t *testing.T) {
		ctx := t.Context()
		store := payloadFleet(t)
		first, err := store.Activate(ctx, coord.ActivationRequest{
			RevisionID: "rev-a", Payload: []byte(`{"v":"a"}`), At: time.Now()})
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		// What a later writer's mirror leaves, planted: its flip is one
		// this read cannot see yet.
		later := uint64(first.Epoch) + 1000
		put(t, store, legacyPayloadKey, legacyPayloadRecord{
			RevisionID: "rev-later", Payload: []byte(`{"v":"later"}`), Epoch: later})
		if err := store.mirrorLegacyPayload(ctx, uint64(first.Epoch), "rev-a",
			[]byte(`{"v":"a"}`)); err != nil {
			t.Fatalf("mirror: %v", err)
		}
		if got := held(t, store); got.RevisionID != "rev-later" || got.Epoch != later {
			t.Errorf("a slow writer replaced a later mirror with %s at epoch %d",
				got.RevisionID, got.Epoch)
		}
	})

	t.Run("an earlier build's later pointer", func(t *testing.T) {
		ctx := t.Context()
		store := payloadFleet(t)
		mine, err := store.Activate(ctx, coord.ActivationRequest{
			RevisionID: "rev-new", Payload: []byte(`{"v":"new"}`), At: time.Now()})
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		// The earlier build activates after it, as it always did: the
		// body beside the pointer first, with no epoch, then a pointer
		// with no body of its own.
		put(t, store, legacyPayloadKey, map[string]any{
			"revision_id": "rev-old", "payload": []byte(`{"v":"old"}`)})
		put(t, store, activationKey, map[string]any{
			"revision_id": "rev-old", "at": time.Now().UTC()})
		if err := store.mirrorLegacyPayload(ctx, uint64(mine.Epoch), "rev-new",
			[]byte(`{"v":"new"}`)); err != nil {
			t.Fatalf("mirror: %v", err)
		}
		if got := held(t, store); got.RevisionID != "rev-old" {
			t.Errorf("a slow writer replaced the body an earlier build's pointer "+
				"names with %s", got.RevisionID)
		}
		if body, found, err := store.Payload(ctx, "rev-old"); err != nil || !found ||
			string(body) != `{"v":"old"}` {
			t.Errorf("the earlier build's revision reads %s found=%v err=%v", body, found, err)
		}
	})
}

// A POINTER AN OLDER BUILD WROTE STILL NAMES A BODY THIS ONE CAN READ.
//
// A bucket written before the body moved into the pointer holds a pointer with
// no payload and the body under its own key, as a base64 string. That is the
// state a node upgraded in place wakes up to, and the pointer is not rewritten
// until somebody next activates — so a build that could not read the body
// beside it would refuse to rejoin the fleet it had just been part of. And the
// revision id on that key is still checked, because every activation, the
// losers of a race included, overwrote it.
//
// Mutation: drop the legacy read and the first half answers "not found".
func TestAnOlderPointersBodyIsReadFromBesideIt(t *testing.T) {
	t.Parallel()
	store := payloadFleet(t)
	ctx := t.Context()
	body := []byte(`{"__encrypted__":"enc:v1:k1:b2xkZXI="}`)
	plant := func(key string, v any) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.config.Put(ctx, key, raw); err != nil {
			t.Fatalf("plant %s: %v", key, err)
		}
	}
	// Exactly the two records such a build wrote: the pointer with no
	// payload field, and the body as bytes.
	plant(activationKey, map[string]any{"revision_id": "rev-old", "at": time.Now().UTC()})
	plant(legacyPayloadKey, legacyPayloadRecord{RevisionID: "rev-old", Payload: body})
	raw, _ := store.config.Get(ctx, legacyPayloadKey)
	if !strings.Contains(string(raw.Value()), `"payload":"`) {
		t.Fatalf("the planted legacy record is not the base64 form: %s", raw.Value())
	}

	if target, found, err := store.Target(ctx); err != nil || !found || target.RevisionID != "rev-old" {
		t.Fatalf("Target over an older pointer = %+v found=%v err=%v", target, found, err)
	}
	got, found, err := store.Payload(ctx, "rev-old")
	if err != nil || !found {
		t.Fatalf("the older pointer's body is unreadable: found=%v err=%v", found, err)
	}
	if string(got) != string(body) {
		t.Errorf("payload = %s, want %s", got, body)
	}

	// THE CONTROL: the same key holding another revision's body is absent,
	// not served — which is the race the legacy shape could lose.
	plant(legacyPayloadKey, legacyPayloadRecord{RevisionID: "rev-racer", Payload: []byte(`{"v":2}`)})
	if got, found, err := store.Payload(ctx, "rev-old"); err != nil || found {
		t.Errorf("another revision's body was served for rev-old: %s found=%v err=%v",
			got, found, err)
	}

	// And the next activation replaces the older shape outright: its body
	// is the pointer's own, and the key beside it now mirrors that body
	// rather than keeping the superseded one — the bucket has no
	// retention, so a body left there would be kept for ever.
	if _, err := store.Activate(ctx, coord.ActivationRequest{
		RevisionID: "rev-new", Payload: []byte(`{"v":3}`), At: time.Now()}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got, found, err := store.Payload(ctx, "rev-new"); err != nil || !found || string(got) != `{"v":3}` {
		t.Errorf("rev-new = %s found=%v err=%v", got, found, err)
	}
	if revision, body, ok := earlierBuildRead(t, store); !ok || revision != "rev-new" ||
		string(body) != `{"v":3}` {
		t.Errorf("the key beside the pointer holds %s = %s (ok=%v), want rev-new's body",
			revision, body, ok)
	}
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
		RateWindow: time.Minute, ClaimTTL: time.Minute, SetupOnceRetention: time.Minute,
		LedgerRetention: time.Minute, FireRetention: time.Minute,
		FollowRetention: time.Minute,
		CooldownMax:     time.Minute, StatusFreshness: time.Minute,
		BudgetRetention: time.Minute,
		BucketPrefix:    prefix,
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

// THE CLASS IS A SUBJECT TOKEN, which is the whole reason a resource key is
// segmented rather than one escaped blob.
//
// Before it was, `seat:alice` became the single token `seat=3Aalice`; a subject
// wildcard matches whole tokens, so there was no filter that selected the
// seats and every class listing read the entire bucket and discarded the rest.
// This asserts the property directly, on the KEY, because it is what the
// broker matches on and a listing that happened to be correct while the key
// was one token would prove nothing about the filter.
func TestAResourceClassIsItsOwnSubjectToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		resource string
		want     string
	}{
		{"seat:alice", "seat.alice"},
		{"node:node-0", "node.node-0"},
		{"worker:scheduler", "worker.scheduler"},
		// A name carrying the separator is MORE segments, never a class
		// with a colon in it — the class is the leading one either way.
		{"bulk:proj:7", "bulk.proj.7"},
		// And a name carrying a dot keeps it escaped, because an
		// unescaped one would add a token the grammar never wrote.
		{"seat:alice.smith", "seat.alice=2Esmith"},
	}
	for _, c := range cases {
		if got := encodeResource(c.resource); got != c.want {
			t.Errorf("encodeResource(%q) = %q, want %q", c.resource, got, c.want)
		}
	}

	// And the filter a class builds selects its own keys and no others —
	// including the adversarial pair, where one class name is a string
	// prefix of another. A subject wildcard matches per TOKEN, so `node.>`
	// does not take `node-pool.x`; a string prefix would.
	seat := coord.DocumentFilter(string(coord.ClassSeat))
	for _, c := range []struct {
		resource string
		want     bool
	}{
		{"seat:alice", true},
		{"seat:alice.smith", true},
		{"node:node-0", false},
		{"worker:seat", false},
		{"seatbelt:x", false},
	} {
		if got := subjectMatches(seat, encodeResource(c.resource)); got != c.want {
			t.Errorf("filter %q vs %q (key %q) = %v, want %v",
				seat, c.resource, encodeResource(c.resource), got, c.want)
		}
	}
}

// subjectMatches is NATS subject matching over the two wildcards, written out
// here because the assertion above is about what the BROKER will do and a test
// that asked the client's own helper would be asserting nothing the server
// promises.
func subjectMatches(filter, subject string) bool {
	f, s := strings.Split(filter, "."), strings.Split(subject, ".")
	for i, tok := range f {
		if tok == ">" {
			return i <= len(s)
		}
		if i >= len(s) {
			return false
		}
		if tok != "*" && tok != s[i] {
			return false
		}
	}
	return len(f) == len(s)
}

// THE BROKER NARROWS THE CLASS READS, and this measures it rather than
// trusting the filter.
//
// The two reads it covers are the ones paid on a ticker: the membership read
// the sweep takes every five seconds, and the placement hints it takes beside
// them — that one over the epochs bucket, which has NO TTL and therefore holds
// a record for every resource the deployment has ever leased. Both used to
// read their whole bucket and discard what they did not want.
//
// The count is taken from INSIDE the walk, because the rows it yields were
// always correct: a filter that did not narrow would return the same leases
// and simply move everything else over the wire to get there.
func TestAClassReadMovesOnlyItsOwnClass(t *testing.T) {
	nc := embeddedNATS(t)
	s := openStore(t, nc, time.Minute)
	ctx := context.Background()

	for _, r := range []string{"seat:ceo", "seat:eng", "seat:ops", "worker:scheduler"} {
		if _, err := s.TryAcquire(ctx, r, coord.AcquireOptions{
			Owner: "node-a", TTL: time.Minute, Preferred: "node-a",
		}); err != nil {
			t.Fatalf("claim %s: %v", r, err)
		}
	}
	if _, err := s.TryAcquire(ctx, coord.NodeResource("node-a"), coord.AcquireOptions{
		Owner: "node-a:1", TTL: time.Minute, Ungated: true,
	}); err != nil {
		t.Fatalf("claim presence: %v", err)
	}

	count := func(b *leaderBucket, class coord.Class) int {
		t.Helper()
		n := 0
		if err := s.eachUnder(ctx, b, class, "the "+string(class)+" records",
			func(jetstream.KeyValueEntry) error { n++; return nil }); err != nil {
			t.Fatalf("walk %s: %v", class, err)
		}
		return n
	}

	// Five resources across three classes, each in the lease bucket its
	// class is written to: seats and presence in the seat lease bucket, the
	// duty in the duty bucket. The epochs bucket holds all five whichever
	// lease bucket the lease itself is in, which is what keeps a duty's
	// counter monotonic across the move between them.
	for _, c := range []struct {
		lane  *lane
		class coord.Class
		want  int
	}{
		{s.leases, coord.ClassSeat, 3},
		{s.leases, coord.ClassNode, 1},
		{s.duties, coord.ClassWorker, 1},
	} {
		if got := count(c.lane.kv, c.class); got != c.want {
			t.Errorf("the %s lease walk was handed %d records for the %d it wanted; "+
				"the broker is not filtering and the membership read is moving "+
				"every seat in the fleet", c.class, got, c.want)
		}
		if got := count(s.epochs, c.class); got != c.want {
			t.Errorf("the %s epoch walk was handed %d records for the %d it wanted; "+
				"that bucket has no TTL, so an unnarrowed read here grows with "+
				"the deployment's whole history", c.class, got, c.want)
		}
	}

	// AND THE SEAT LEASE BUCKET CARRIES NO DUTY OF THIS BUILD'S, which is
	// the other half of what keeps the membership read cheap: a class the
	// bucket does not hold is a bucket the read never opens at all.
	if got := count(s.leases.kv, coord.ClassWorker); got != 0 {
		t.Errorf("the seat lease bucket holds %d duty records; this build writes "+
			"every duty it claims to the duty bucket", got)
	}

	// And the answers are still right, which is the half a narrowing bug
	// would not disturb.
	live, err := s.ListLive(ctx, coord.ClassNode)
	if err != nil || len(live) != 1 {
		t.Fatalf("ListLive(node) = %v, %v; want the one presence lease", live, err)
	}
	hints, err := s.PreferredResources(ctx, coord.ClassSeat, "node-a")
	if err != nil || len(hints) != 3 {
		t.Fatalf("PreferredResources(seat) = %v, %v; want the three seat hints", hints, err)
	}
}

// A SECOND NODE ADOPTS THE LEASE BUCKET RATHER THAN REWRITING IT, and is
// honest about the TTL that is actually in force.
//
// Open used to call CreateOrUpdateKeyValue, which makes every booting node's
// call a WRITE: the losers of the create race rewrote a configuration they
// already agreed with against a metadata group that was still electing, which
// is the shape this package removed from every other bucket. The consequence
// when the two disagree is worse than the write: whichever node booted LAST
// silently redefined how long every other node's leases lived.
//
// So the bucket is adopted, and the store carries the live TTL. Believing the
// configured one instead would let validateTTL accept claims the bucket will
// not honour — a deadline handed back that is a lie about when the lease ends.
func TestASecondOpenAdoptsTheLeaseTTLInForce(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("t%d", bucketSeq.Add(1))

	const inForce = 90 * time.Second
	first, err := Open(context.Background(), nc, Config{TTL: inForce, BucketPrefix: prefix})
	if err != nil {
		t.Fatalf("the first Open: %v", err)
	}
	if first.TTL() != inForce {
		t.Fatalf("the node that created the bucket reports %v, want %v",
			first.TTL(), inForce)
	}

	// THE SECOND NODE ASKS FOR SOMETHING ELSE, which is what N nodes
	// holding possibly-different Tier A files actually do.
	second, err := Open(context.Background(), nc, Config{TTL: 30 * time.Second, BucketPrefix: prefix})
	if err != nil {
		t.Fatalf("a second Open against an existing bucket: %v", err)
	}
	if second.TTL() != inForce {
		t.Errorf("the second node reports a lease TTL of %v; the bucket's is "+
			"%v, and a store that believes its own config here hands back "+
			"deadlines the bucket will not honour", second.TTL(), inForce)
	}

	// AND THE BUCKET ITSELF IS UNCHANGED — the second node wrote nothing.
	status, err := statusOf(context.Background(), second.leases.kv)
	if err != nil {
		t.Fatalf("read the lease bucket's status: %v", err)
	}
	if status.TTL() != inForce {
		t.Errorf("the lease bucket's TTL is now %v: the second node rewrote a "+
			"configuration it does not own", status.TTL())
	}
}

// A BUCKET REPLICATED BELOW WHAT THIS NODE IS CONFIGURED FOR IS REFUSED.
//
// # Why this one difference is fatal where the lease TTL is only warned about
//
// Because it is the difference with no symptom. Every other way a running
// bucket can disagree with this node's Tier A changes how the store BEHAVES,
// and behaviour is observable: a shorter lease TTL hands seats around sooner,
// and the case above is about reporting the one in force rather than the one
// configured. Replication changes nothing until a node is lost — and then it
// changes everything, because the leases, the fencing epochs and the company's
// secrets were on one disk the whole time while every node reported itself
// correctly configured for three.
//
// An operator who raises stream.replicas on an existing fleet gets a
// rolling restart in which each node finds its buckets already there, adopts
// them, and goes on running single-replica coordination. Nothing writes to a
// bucket that exists — that is the rule openBucket is built on — so the only
// honest move left is to say so.
func TestABucketReplicatedBelowThisNodesConfigIsRefused(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("t%d", bucketSeq.Add(1))

	// THE FLEET STARTED AT ONE REPLICA, which is what a single-node
	// deployment or an early cluster actually has.
	if _, err := Open(context.Background(), nc, Config{TTL: 45 * time.Second, BucketPrefix: prefix, Replicas: 1}); err != nil {
		t.Fatalf("the first Open: %v", err)
	}

	// AND THE OPERATOR RAISED IT. The buckets are still the ones made at
	// one replica, and no node rewrites them.
	_, err := Open(context.Background(), nc, Config{TTL: 45 * time.Second, BucketPrefix: prefix, Replicas: 3})
	if err == nil {
		t.Fatal("a node configured for 3 replicas adopted single-replica " +
			"coordination and reported itself healthy — the leases, the " +
			"fencing epochs and the company's secrets are on one disk and " +
			"nothing says so")
	}
	// THE COUNTS ARE IN THE MESSAGE, because "replication mismatch" leaves
	// an operator unable to tell which side is the one to change.
	// THE FIELD IS stream.replicas, which is the one an operator can grep
	// their Tier A for: the coordination buckets take their replica count
	// from it because they live on the same broker, and there is no
	// coordination.replicas to go and look for.
	for _, want := range []string{"1x", "3x", "stream.replicas"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	// AND EQUAL OR HIGHER STILL STARTS, so a single-replica development
	// node against a replicated fleet's buckets is not locked out.
	if _, err := Open(context.Background(), nc, Config{TTL: 45 * time.Second, BucketPrefix: prefix, Replicas: 1}); err != nil {
		t.Errorf("a node configured for fewer replicas than the bucket has "+
			"was refused: %v", err)
	}
}

// AN ADMITTED CHARGE CLEARS ONLY THE REFUSAL IT SAW.
//
// The contract suite cannot reach this interleaving: a charge is admitted
// while the scope carries a refusal, another caller is refused and stamps a
// newer one, and only then does the first caller clear. Its stamp is stale by
// then, and the newer refusal is still true, so a clear that ignored which
// stamp it saw would hide a scope that is refusing right now.
func TestAnAdmittedChargeLeavesANewerRefusalStanding(t *testing.T) {
	store := openFleet(t, embeddedNATS(t))
	ctx := t.Context()
	w := testWindows()
	day := []period.Period{period.Day}
	stamp := func() coord.Tally {
		t.Helper()
		tally, err := store.tally(ctx, coord.OrgScope, w)
		if err != nil {
			t.Fatalf("tally: %v", err)
		}
		return tally
	}

	if err := store.stampRefusal(ctx, coord.OrgScope, day, coord.Tally{}.Roll(w), w); err != nil {
		t.Fatalf("stampRefusal: %v", err)
	}
	seen := stamp()
	// A distinct instant, so the two stamps cannot compare equal by
	// landing in the same clock tick.
	time.Sleep(2 * time.Millisecond)
	if err := store.stampRefusal(ctx, coord.OrgScope, day, seen, w); err != nil {
		t.Fatalf("stampRefusal: %v", err)
	}
	newer := stamp()
	if !newer.Slots[0].RefusedAt.After(seen.Slots[0].RefusedAt) {
		t.Fatalf("setup: the second stamp %v is not after the first %v",
			newer.Slots[0].RefusedAt, seen.Slots[0].RefusedAt)
	}

	store.clearRefusal(ctx, coord.OrgScope, seen)
	if got := stamp().Slots[0].RefusedAt; !got.Equal(newer.Slots[0].RefusedAt) {
		t.Fatalf("refusal stamp = %v, want the newer %v: a stale clear erased a "+
			"refusal that is still true", got, newer.Slots[0].RefusedAt)
	}

	store.clearRefusal(ctx, coord.OrgScope, newer)
	if got := stamp().Slots[0].RefusedAt; !got.IsZero() {
		t.Fatalf("refusal stamp = %v, want it cleared by a caller that saw it", got)
	}
}

// testWindows is the windows these cases charge in: a fixed day in the past,
// so no case straddles a boundary by starting near midnight.
func testWindows() coord.Windows {
	return coord.WindowsAt(time.Date(2026, time.March, 14, 15, 9, 26, 0, time.UTC), time.UTC)
}

// orgSpent is the org's day, week and month spend at testWindows.
func orgSpent(t *testing.T, store *FleetStore) [3]int {
	t.Helper()
	u, err := store.Used(t.Context(), coord.OrgScope, testWindows())
	if err != nil {
		t.Fatalf("Used: %v", err)
	}
	return [3]int{u.Windows[0].Used, u.Windows[1].Used, u.Windows[2].Used}
}

// AN UNREACHABLE COUNTER IS AN ERROR, NEVER A REFUSAL.
//
// "The company is out of tokens" is a budget event an operator acts on and
// "the counter could not be read" is an outage, and a caller that cannot tell
// them apart either stops a healthy company over a blip or spends past a cap
// it could not read. A dead context is the one unreachable store a test can
// make on demand; the answer it gets must carry the error and no refusal.
func TestAnUnreachableCounterIsAnErrorNeverARefusal(t *testing.T) {
	store := openFleet(t, embeddedNATS(t))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := store.Charge(ctx, coord.ChargeRequest{
		Seat: coord.AgentScope("x"), Tokens: 10, Windows: testWindows(),
		OrgCaps: coord.Caps{period.Day: 100},
	})
	if err == nil {
		t.Fatalf("Charge on a dead context = %+v with no error", got)
	}
	if got.OK || got.RefusedScope != "" {
		t.Fatalf("Charge on a dead context = %+v: an outage reported as a decision", got)
	}
	if _, err := store.Used(ctx, coord.OrgScope, testWindows()); err == nil {
		t.Fatal("Used on a dead context answered a figure rather than an error")
	}
}

// A CHARGE WHOSE CALLER HANGS UP STILL UNWINDS THE COMPANY'S HALF.
//
// The org is written before the seat, and a seat write that fails is undone by
// taking the org's tokens back off. The failure being undone is often the
// caller's own cancellation, and an unwind that inherited that dead context
// failed with it: the company was billed for a round that never ran, and kept
// refusing early until its windows turned over.
func TestACancelledChargeStillUnwindsTheOrg(t *testing.T) {
	store := openFleet(t, embeddedNATS(t))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store.budgets.client = hangUpAfterWriting{
		clientBucket: store.budgets.client, key: encodeKey(coord.OrgScope), hangUp: cancel,
	}

	if got, err := store.Charge(ctx, coord.ChargeRequest{
		Seat: "agent:x", Tokens: 10, Windows: testWindows(),
		OrgCaps: coord.Caps{period.Day: 100}, SeatCaps: coord.Caps{period.Day: 100},
	}); err == nil {
		t.Fatalf("Charge = %+v, want the seat's write to fail on the cancelled context", got)
	}
	if used := orgSpent(t, store); used != [3]int{} {
		t.Errorf("org day/week/month = %v after a charge that failed, want nothing: "+
			"the unwind ran on the cancelled context and left the company billed", used)
	}
}

// A POST-CHARGE IS ALL OR NOTHING, exactly as a charge is.
//
// Two keys and no transaction, so the property is built rather than given: the
// org is written first and taken back when the seat's write fails. Without the
// compensation a collected coding run whose second write failed would leave the
// company billed for tokens the seat's own counter never saw, and the caller —
// which retries — would bill the org again.
func TestAPostChargeThatCannotFinishRecordsNeitherScope(t *testing.T) {
	store := openFleet(t, embeddedNATS(t))
	seat := coord.AgentScope("x")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store.budgets.client = hangUpAfterWriting{
		clientBucket: store.budgets.client, key: encodeKey(coord.OrgScope), hangUp: cancel,
	}

	if got, err := store.PostCharge(ctx, seat, 10, testWindows()); err == nil {
		t.Fatalf("PostCharge = %+v, want the seat's write to fail on the cancelled context", got)
	}
	if used := orgSpent(t, store); used != [3]int{} {
		t.Errorf("org day/week/month = %v after a post-charge that failed, want "+
			"nothing: the unwind ran on the cancelled context and left the company "+
			"billed for a run its seat never recorded", used)
	}
}

// AN UNWIND TAKES A CHARGE BACK FROM THE WINDOW IT WAS COUNTED IN.
//
// The seat's write can fail after midnight has passed and a peer has rolled
// the org's day. What the refused round spent belongs to the day that is over;
// taking it off the new day would hand that day credit, and a floor at zero
// would hide it only until the day's first real charge.
func TestAnUnwindLeavesAWindowThatHasRolledOnAlone(t *testing.T) {
	store := openFleet(t, embeddedNATS(t))
	ctx := t.Context()
	today := testWindows()
	tomorrow := coord.WindowsAt(today[0].End, time.UTC)

	charged, fits, err := store.bump(ctx, coord.OrgScope, 40, today, nil)
	if err != nil || !fits {
		t.Fatalf("bump = (%v, %v)", fits, err)
	}
	// A peer's charge crosses midnight before the unwind lands.
	if _, _, err := store.bump(ctx, coord.OrgScope, 25, tomorrow, nil); err != nil {
		t.Fatalf("bump tomorrow: %v", err)
	}
	store.unwindOrg(ctx, 40, charged)

	u, err := store.Used(ctx, coord.OrgScope, tomorrow)
	if err != nil {
		t.Fatalf("Used: %v", err)
	}
	if got := u.Windows[0].Used; got != 25 {
		t.Errorf("tomorrow's day = %d, want the 25 charged in it: the unwind took "+
			"yesterday's round off a window it was never counted in", got)
	}
	// The week and the month did not roll, so the round comes off them.
	if got := u.Windows[2].Used; got != 25 {
		t.Errorf("the month = %d, want 25: the unwound round is still counted", got)
	}
}

// AN ADMITTED CHARGE CLEARS THE STAMP EVEN IF THE CALLER HAS HUNG UP.
//
// The clear runs after BOTH counters are written, so the charge is a fact by
// then and the caller's context dying in between is ordinary — a turn
// cancelled, a node draining. Left on that context the clear failed with it,
// and the scope went on telling every dashboard it was refusing charges while
// it had just admitted one, until the next admitted charge on a live context.
func TestACancelledChargeStillClearsTheRefusalItAdmittedPast(t *testing.T) {
	store := openFleet(t, embeddedNATS(t))
	seat := coord.AgentScope("x")
	w := testWindows()
	day := []period.Period{period.Day}
	for _, scope := range []string{coord.OrgScope, seat} {
		if err := store.stampRefusal(t.Context(), scope, day, coord.Tally{}.Roll(w), w); err != nil {
			t.Fatalf("stampRefusal(%s): %v", scope, err)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The SEAT's write is the last one before the clears, so a hang-up
	// there leaves both counters written and both clears to make.
	store.budgets.client = hangUpAfterWriting{
		clientBucket: store.budgets.client, key: encodeKey(seat), hangUp: cancel,
	}

	if got, err := store.Charge(ctx, coord.ChargeRequest{
		Seat: seat, Tokens: 10, Windows: w,
		OrgCaps: coord.Caps{period.Day: 100}, SeatCaps: coord.Caps{period.Day: 100},
	}); err != nil || !got.OK {
		t.Fatalf("Charge = (%+v, %v), want it admitted", got, err)
	}
	rows, err := store.Usage(t.Context(), w)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	for _, row := range rows {
		if !row.Windows[0].RefusedAt.IsZero() {
			t.Errorf("%s still reads as refusing after an admitted charge: the "+
				"clear ran on the caller's cancelled context", row.Scope)
		}
	}
}

// THE WINDOWED COUNTERS AGE, AND THE AGE IS THE CONFIGURED ONE.
//
// A counter bucket with no age would keep a record for every seat that ever
// ran, for the life of the deployment. This one reaps a record nobody has
// charged for longer than the longest window — here a bucket built with a
// short retention, so the case can watch it happen.
func TestAnUnchargedCounterAgesOut(t *testing.T) {
	nc := embeddedNATS(t)
	store := openFleetWithTTL(t, nc, 500*time.Millisecond)
	ctx := t.Context()
	if _, err := store.PostCharge(ctx, coord.AgentScope("x"), 10, testWindows()); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, err := store.Usage(ctx, testWindows())
		if err != nil {
			t.Fatalf("Usage: %v", err)
		}
		if len(rows) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the counters are still listed %v after their bucket's age: %+v",
				10*time.Second, rows)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// THE COUNTER RECORD'S WIRE IS SHARED BY EVERY BUILD ON THE BUCKET, and its
// failure is the open one. A record whose slot key a reader does not know
// decodes as a slot with no label, which every charge rolls — so a renamed key
// would hand each scope its whole allowance back on the first charge a peer of
// the other spelling made. Pinned as bytes, both ways: what this build writes,
// and that what a peer wrote reads back as the same counter.
func TestTheCounterRecordNamesEachSlotByItsLabel(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, time.March, 14, 12, 0, 0, 0, time.UTC)
	refused := at.Add(time.Minute)
	tally := coord.Tally{}.Roll(coord.WindowsAt(at, time.UTC)).Add(60, at)
	tally = tally.Stamp([]period.Period{period.Week}, tally, refused)

	const wire = `{"slots":{"day":{"label":"2026-03-14","used":60},` +
		`"month":{"label":"2026-03","used":60},` +
		`"week":{"label":"2026-W11","used":60,"refused_at":"2026-03-14T12:01:00Z"}},` +
		`"at":"2026-03-14T12:00:00Z"}`
	raw, err := encodeTally(tally)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(raw) != wire {
		t.Fatalf("the counter record is\n  %s\nwant\n  %s", raw, wire)
	}
	back, err := decodeTally([]byte(wire))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Slots != tally.Slots || !back.At.Equal(tally.At) {
		t.Fatalf("a peer's record read back as %+v, want %+v", back, tally)
	}
}

// openFleet opens a fleet store on its own buckets, for a test that needs to
// reach inside one rather than run the contract suite over it.
func openFleet(t *testing.T, nc *nats.Conn) *FleetStore {
	t.Helper()
	store, err := OpenFleet(context.Background(), nc, FleetConfig{
		RateWindow: time.Minute, ClaimTTL: time.Minute, SetupOnceRetention: time.Minute,
		LedgerRetention: time.Minute, FireRetention: time.Minute,
		FollowRetention: time.Minute,
		CooldownMax:     time.Minute, StatusFreshness: time.Minute,
		BudgetRetention: time.Minute,
		BucketPrefix:    fmt.Sprintf("f%d", bucketSeq.Add(1)),
	})
	if err != nil {
		t.Fatalf("OpenFleet: %v", err)
	}
	return store
}

// hangUpAfterWriting is the budgets bucket's client handle with one fault
// injected: the moment one key is written, the caller's context is cancelled,
// the way a caller hanging up between the org's write and the seat's makes the
// second fail.
//
// UPDATE IS THE ONE WRITE TO WRAP, because a create here is a conditional
// publish at revision zero through it (see [leaderBucket.Create]) — so a
// create and an increment of the key both land on it.
type hangUpAfterWriting struct {
	clientBucket
	key    string
	hangUp context.CancelFunc
}

func (k hangUpAfterWriting) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	rev, err := k.clientBucket.Update(ctx, key, value, revision)
	if err == nil && key == k.key {
		k.hangUp()
	}
	return rev, err
}
