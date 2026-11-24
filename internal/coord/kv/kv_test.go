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
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// jsOf is a JetStream client over nc for these tests' brokers, started with
// no domain, so it addresses the account's own JetStream.
func jsOf(nc *nats.Conn) jetstream.JetStream {
	js, err := jsapi.Account().Client(nc)
	if err != nil {
		panic(err)
	}
	return js
}

// embeddedNATS starts a listenerless nats-server in the test process, the
// topology internal/queue/jetstream boots for a solo node.
func embeddedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	_, nc := embeddedServer(t)
	return nc
}

// embeddedServer is [embeddedNATS] with the server beside the connection, for
// a case that reads what the broker itself counts.
func embeddedServer(t testing.TB) (*server.Server, *nats.Conn) {
	t.Helper()
	dir := brokertest.TempDir(t)
	ns, err := server.NewServer(&server.Options{
		ServerName:        "coordkv-test",
		JetStream:         true,
		Port:              -1,
		DontListen:        true,
		StoreDir:          dir,
		JetStreamMaxStore: brokertest.StoreMaxBytes,
		// THE ENGINE'S TRANSPORT CEILING: a value at coord.MaxValueBytes
		// is a 6 MiB message, past the server's 1 MiB default.
		MaxPayload: int32(queue.MaxPayloadBytes),
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
	return ns, nc
}

// bucketSeq gives every store its own set of buckets.
var bucketSeq atomic.Int64

func openStore(t *testing.T, nc *nats.Conn, ttl time.Duration) *Store {
	t.Helper()
	return openStoreVia(t, jsOf(nc), InProcess, ttl)
}

// openStoreVia is [openStore] over a client in any API, which reaches the
// buckets as reach says.
func openStoreVia(t *testing.T, client jetstream.JetStream, reach Reach, ttl time.Duration) *Store {
	t.Helper()
	prefix := fmt.Sprintf("t%d", bucketSeq.Add(1))
	s, err := Open(context.Background(), client, client, Config{TTL: ttl, BucketPrefix: prefix, Reach: reach})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// TestContract runs the one suite every coord.Backend is certified by.
//
// Two things about how it is wired here.
//
// The TTL is coordtest.LongTTL because the bucket's MaxAge must cover the
// longest TTL a case asks for, and this backend refuses a TTL it cannot honour.
// Shorter leases lapse by the record's deadline on the STORE's clock
// (Store.storeNow). The duty cases claim at coord.MaxDutyTTL, which the duty
// bucket honours whatever this configuration's TTL.
//
// There is no coordtest.Advancer, since a real broker's clock cannot be moved:
// harness.lapse sleeps ShortTTL + margin, overlapped across parallel cases.
func TestContract(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	coordtest.Run(t, func(t *testing.T) coord.Backend {
		return openStore(t, nc, coordtest.LongTTL)
	})
}

// TestMintingContract runs the held-mid-mint cases over the KV store, held
// through its test-only mint hook.
func TestMintingContract(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	coordtest.RunMinting(t, func(t *testing.T, hold coordtest.MintHold) coordtest.BrowserControlStore {
		s := openStore(t, nc, coordtest.LongTTL)
		s.beforeMint = hold
		return s
	})
}

// TestSharedContract runs the shared cases over three handles on one
// single-member broker, the easy case; shared_cluster_test.go runs them on
// three members of a cluster.
func TestSharedContract(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	coordtest.RunShared(t, func(t *testing.T) []coord.Backend {
		prefix := fmt.Sprintf("t%d", bucketSeq.Add(1))
		out := make([]coord.Backend, 3)
		for i := range out {
			s, err := Open(t.Context(), jsOf(nc), jsOf(nc), Config{
				TTL: coordtest.LongTTL, BucketPrefix: prefix, Reach: InProcess,
			})
			if err != nil {
				t.Fatalf("Open handle %d: %v", i, err)
			}
			out[i] = s
		}
		return out
	})
}

// TestBrowserControlContract runs the browser control suite over three handles
// on one single-member broker; shared_cluster_test.go runs it on members of a
// cluster that hold no copy of the duty bucket the class lives in.
func TestBrowserControlContract(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	coordtest.RunBrowserControls(t, func(t *testing.T) []coordtest.BrowserControlStore {
		prefix := fmt.Sprintf("t%d", bucketSeq.Add(1))
		out := make([]coordtest.BrowserControlStore, 3)
		for i := range out {
			s, err := Open(t.Context(), jsOf(nc), jsOf(nc), Config{
				TTL: coordtest.LongTTL, BucketPrefix: prefix, Reach: InProcess,
			})
			if err != nil {
				t.Fatalf("Open handle %d: %v", i, err)
			}
			out[i] = s
		}
		return out
	})
}

func TestKeyMappingRoundTrips(t *testing.T) {
	t.Parallel()
	// A dot matters most: it is a SUBJECT SEPARATOR in a NATS key, so an
	// unescaped one would silently split the key into two tokens.
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
		"merge:a project:7",
	}
	seen := map[string]string{}
	for _, r := range resources {
		key := encodeResource(r)
		if !validKey(key) {
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

// TestAwkwardResourceNamesSurviveTheStore proves nothing between
// encodeKey and decodeKey (the listings, the prefix filters, the buckets)
// reads a resource by anything but its real name.
func TestAwkwardResourceNamesSurviveTheStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	s := openStore(t, nc, time.Minute)

	// BUILT FROM THE CLASS DIRECTLY: coord.SeatResource takes a uuid and
	// cannot produce a dotted name, but a company, a duty or a stream name
	// reaching these buckets can carry a dot.
	dotted := coord.ClassSeat.Resource("alice.smith")
	plain := coord.ClassSeat.Resource("alice")
	for _, r := range []string{dotted, plain} {
		if _, _, err := s.TryAcquire(ctx, r, coord.AcquireOptions{
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
		{"no TTL", Config{Reach: InProcess}},
		{"TTL below the broker's floor", Config{TTL: 50 * time.Millisecond, Reach: InProcess}},
		{"negative TTL", Config{TTL: -time.Second, Reach: InProcess}},
		{"bucket prefix with a dot", Config{TTL: time.Minute, BucketPrefix: "a.b", Reach: InProcess}},
		{"bucket prefix with a colon", Config{TTL: time.Minute, BucketPrefix: "a:b", Reach: InProcess}},
		{"too many replicas", Config{TTL: time.Minute, Replicas: 9, Reach: InProcess}},
		{"no reach", Config{TTL: time.Minute}},
		{"a reach nothing knows", Config{TTL: time.Minute, Reach: "nearby"}},
	}
	for _, c := range cases {
		if _, err := Open(context.Background(), jsOf(nc), jsOf(nc), c.cfg); err == nil {
			t.Fatalf("Open with %s was accepted", c.name)
		}
	}
	if _, err := Open(context.Background(), nil, nil, Config{TTL: time.Minute, Reach: InProcess}); err == nil {
		t.Fatal("Open with no connection was accepted")
	}
}

func TestATTLLongerThanTheBucketIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	s := openStore(t, nc, time.Minute)

	// An ERROR, not (nil, nil): nobody else holds the seat; the caller
	// asked for what the bucket cannot promise.
	lease, _, err := s.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: 2 * time.Minute})
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
	// Exactly the configured TTL is accepted.
	if _, _, err := s.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: time.Minute}); err != nil {
		t.Fatalf("TryAcquire at exactly the bucket TTL: %v", err)
	}
}

// TestServerSideExpiryHandsTheSeatOver is the production shape the contract
// suite cannot reach: every lease at the bucket's TTL, so the record's
// disappearance is the expiry and no clock is consulted.
func TestServerSideExpiryHandsTheSeatOver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	const ttl = time.Second
	s := openStore(t, nc, ttl)

	first, _, err := s.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{
		Owner: "node-a:1", TTL: ttl, Preferred: "node-a",
	})
	if err != nil || first == nil {
		t.Fatalf("first claim = (%v, %v)", first, err)
	}

	// Renewing through Update refreshes the record's age, the only reason a
	// lease outlives the bucket's TTL.
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

	// The peer's claim lands through Create on the key the broker removed.
	taken, _, err := s.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{Owner: "node-b:1", TTL: ttl})
	if err != nil || taken == nil {
		t.Fatalf("takeover after a server-side expiry = (%v, %v)", taken, err)
	}
	// Why the epoch has its own bucket: the lease key expired and the
	// counter still went up.
	if taken.Epoch <= first.Epoch {
		t.Fatalf("epoch %d after the lease key expired at epoch %d — the counter reset with the key",
			taken.Epoch, first.Epoch)
	}
	// The hint survived too, which a hint on the lease key could not.
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

	lease, _, err := s.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: time.Minute})
	if err != nil || lease == nil {
		t.Fatalf("claim = (%v, %v)", lease, err)
	}
	if ok, err := s.Release(ctx, "seat:ceo", "node-a", lease.Epoch); err != nil || !ok {
		t.Fatalf("release = (%v, %v)", ok, err)
	}
	// A tombstone, not a delete, keeps the resource's history readable while
	// it is unheld.
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

	// An older peer holds a seat, and a presence registration still lands:
	// a newer node missing from membership makes every peer divide the
	// seats by a fleet that excludes it.
	if _, _, err := s.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{
		Owner: "old:1", TTL: time.Minute, Protocol: 1,
	}); err != nil {
		t.Fatalf("old claim: %v", err)
	}
	presence, _, err := s.TryAcquire(ctx, coord.NodeResource("new"), coord.AcquireOptions{
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
// The retentions are the suite's ([openFleetVia]): what is certified is the
// SEMANTICS, which do not vary with retention.
//
// EVERY NODE ON A CONNECTION OF ITS OWN, because a store coalesces its own
// writes to a key ([writeLines]): contention cases need several stores to have
// several contenders. Through leaves is [TestFleetContractThroughALeaf].
func TestFleetContract(t *testing.T) {
	t.Parallel()
	srv, _ := embeddedServer(t)
	coordtest.RunFleet(t, func(t *testing.T) func() coord.Fleet {
		prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
		return func() coord.Fleet {
			nc, err := nats.Connect("", nats.InProcessServer(srv))
			if err != nil {
				t.Fatalf("connect a node: %v", err)
			}
			t.Cleanup(nc.Close)
			return openFleetForTest(t, nc, prefix)
		}
	})
}

// TestNodeStatusRecordContract runs the node status records' suite over the
// KV: a record store and the lease store its presence joins, on their own
// buckets on one broker, as a node opens both.
func TestNodeStatusRecordContract(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	coordtest.RunNodeStatusRecords(t, func(t *testing.T) (coord.Backend, coord.NodeStatusRecords) {
		return openStore(t, nc, coordtest.LongTTL), openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	})
}

// A NODE'S STATUS RECORD AGES OUT WITH ITS BUCKET, at the apply status's
// freshness: nothing sweeps a dead node's rows, so a bucket with no age keeps
// every node that ever ran.
//
// Mutation: open the node status bucket with no age, or with the ledger's,
// and the age in force reads otherwise.
func TestANodesStatusRecordAgesOutAtTheStatusFreshness(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	status, err := store.nodeStatus.Status(t.Context())
	if err != nil {
		t.Fatalf("read the node status bucket: %v", err)
	}
	if got := status.TTL(); got != time.Minute {
		t.Errorf("the node status bucket ages at %v, want the StatusFreshness it was opened with (%v)",
			got, time.Minute)
	}
}

// ONE NODE'S UNREADABLE ROW IS THAT NODE, OR THAT ROW, NOT SAYING, never every
// node's rows failed: an own row this build cannot decode drops its node, an
// MCP row that does not decode drops that row alone, and a key outside the
// grammar (a bare node id) is no row at all. (The memory twin cannot hold
// one, so the case is here.)
//
// Mutation: keep a row that does not decode as an empty one, and the
// listing draws a server nobody reported.
func TestAnUndecodableStatusRowIsSkippedNotRaised(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	n2 := coord.NodeStatusRecord{Node: "n2", Epoch: 3,
		MCP: []coord.MCPServerStatus{{Server: "github", Started: 1}, {Server: "jira", Started: 1}}}
	if err := coordtest.PutNodeStatus(t.Context(), store, n2); err != nil {
		t.Fatalf("PutNodeStatus: %v", err)
	}
	for key, value := range map[string]string{
		coord.NodeStatusRowKey{Class: coord.NodeStatusOwn, Node: "n1"}.Key():               "not an object",
		coord.NodeStatusRowKey{Class: coord.NodeStatusMCP, Node: "n2", Name: "jira"}.Key(): "[]",
		encodeKey("n3"): `{"epoch":3}`,
	} {
		if _, err := store.nodeStatus.Put(t.Context(), key, []byte(value)); err != nil {
			t.Fatalf("write %s: %v", key, err)
		}
	}
	got, err := store.NodeStatusRecords(t.Context(), coord.NodeStatusRead{MCP: true})
	if err != nil {
		t.Fatalf("NodeStatusRecords over undecodable rows: %v", err)
	}
	if len(got) != 1 || got[0].Node != "n2" || got[0].Epoch != 3 || len(got[0].MCP) != 1 ||
		got[0].MCP[0].Server != "github" {
		t.Errorf("the listing answered %+v, want n2's record with its github row alone", got)
	}
}

// THE BODY RIDES ITS OWN KEY, UNDER ITS DIGEST, AS IT WAS SEALED.
//
// The pointer is read by every probe on every node, so it carries only the
// body's length and digest, and the body is stored raw under `body.<sha256>`.
// The contract suite sees only what Body returns, so the WIRE SHAPE is
// asserted here.
//
// Mutation: carry the payload in coord.ActivationRecord again, and the pointer
// grows by the body; store the body under another key, and it is not found.
func TestThePointerNamesItsBodyAndTheBodyRidesItsOwnKey(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	body := `{"__encrypted__":"enc:v1:k1:q8+/Zm9vYmFy=="}`
	if _, err := store.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-1", Payload: []byte(body), At: time.Now(),
		Origin: coord.RevisionOrigin{
			Author: "maya", AuthorKind: iam.ActorOperator, Source: "api", CreatedAt: time.Now(),
		},
	}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	entry, err := store.get(t.Context(), store.config, activationKey)
	if err != nil {
		t.Fatalf("read the pointer: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(entry.Value(), &fields); err != nil {
		t.Fatalf("decode the pointer: %v", err)
	}
	if _, carried := fields["payload"]; carried || strings.Contains(string(entry.Value()), "enc:v1") {
		t.Errorf("the pointer carries the body itself: %s", entry.Value())
	}
	ref := coord.BodyOf([]byte(body))
	var named coord.BodyRef
	if err := json.Unmarshal(fields["body"], &named); err != nil || named != ref {
		t.Errorf("the pointer names the body %s (%v), want %+v", fields["body"], err, ref)
	}
	stored, err := store.get(t.Context(), store.config, "body."+ref.SHA256)
	if err != nil {
		t.Fatalf("read the body's own key: %v", err)
	}
	if string(stored.Value()) != body {
		t.Errorf("the body's own key holds %q, want the body as sealed", stored.Value())
	}
}

// A POINTER THAT NAMES NO BODY IS READ, AND ITS BODY IS REFUSED, NEVER ABSENT.
//
// Only a build from before bodies had keys wrote one. Target answers it, so a
// node holding the revision keeps running it and `POST /config/reload`
// rewrites it; Body refuses it as coord.ErrBodyless, since answered "absent" a
// converging node would read the pointer again for ever.
//
// Mutation: answer a bodiless pointer's body as not found and the case fails.
func TestAPointerWithNoBodyIsReadAndItsBodyRefused(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	ctx := t.Context()
	raw, err := json.Marshal(map[string]any{"revision_id": "rev-1", "at": time.Now().UTC(),
		"payload": map[string]string{"name": "Acme"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.config.Put(ctx, activationKey, raw); err != nil {
		t.Fatalf("plant the pointer: %v", err)
	}
	target, found, err := store.Target(ctx)
	if err != nil || !found || target.RevisionID != "rev-1" || target.Body.Named() {
		t.Fatalf("Target of a pointer naming no body = %+v found=%v err=%v, want rev-1 naming none",
			target, found, err)
	}
	if got, found, err := store.Body(ctx, target); !errors.Is(err, coord.ErrBodyless) ||
		errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("Body of a pointer naming no body = %s found=%v err=%v, want coord.ErrBodyless", got, found, err)
	}
}

// A BODY THAT IS NOT THE BYTES ITS ACTIVATION NAMES IS A FAULT, NAMING ITS
// KEY, NEVER AN OUTAGE: the key holds them until it is written again. Only a
// hand or another process writes one, which the suite cannot plant.
//
// Mutation: drop the length or the digest check from Body and the forged body
// is served.
func TestAForgedBodyIsRefusedNamingItsKey(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	ctx := t.Context()
	published, err := store.Activate(ctx, coord.ActivationRequest{
		RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: time.Now(),
		Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorOperator, Source: "api",
			CreatedAt: time.Now()},
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	for name, forged := range map[string]string{
		"another length": `{"v":10}`,
		"another digest": `{"v":2}`,
	} {
		if _, err := store.config.Put(ctx, bodyKey(published.Body), []byte(forged)); err != nil {
			t.Fatalf("%s: forge the body: %v", name, err)
		}
		got, found, err := store.Body(ctx, published)
		if !errors.Is(err, coord.ErrBodyCorrupt) || errors.Is(err, coord.ErrUnavailable) ||
			!strings.Contains(err.Error(), bodyKey(published.Body)) {
			t.Errorf("%s: Body = %s found=%v err=%v, want coord.ErrBodyCorrupt naming the key",
				name, got, found, err)
		}
	}
}

// A CORRUPT CREDENTIAL RECORD RAISES RATHER THAN VANISHING.
//
// The contract suite cannot plant one (PutSecret validates). SecretValues IS
// the engine's boot snapshot: a name silently dropped resolves as an empty
// ${VAR} on every node, reaching a provider as an empty credential and an auth
// failure blamed on the vendor. "I could not read it" must be louder than
// "there is none".
func TestAnUndecodableSecretIsRaisedNotSkipped(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
	store, err := OpenFleet(context.Background(), jsOf(nc), FleetConfig{
		Reach:         InProcess,
		MaxRateWindow: time.Minute, MaxClaimTTL: time.Minute,
		LedgerRetention: time.Minute, FireRetention: time.Minute,
		FollowRetention: time.Minute, RebaseRetention: time.Minute,
		StatusFreshness:  time.Minute,
		CustodyRetention: time.Minute,
		BudgetRetention:  time.Minute,
		BucketPrefix:     prefix,
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
	// Straight into the bucket, past the contract's validation.
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
	// The healthy one still reads: the failure is the record's, not the
	// bucket's.
	if _, found, err := store.Secret(ctx, "GOOD"); err != nil || !found {
		t.Fatalf("GOOD: found=%v err=%v", found, err)
	}
}

// THE CLASS IS A SUBJECT TOKEN, which is why a resource key is segmented
// rather than one escaped blob: a subject wildcard matches whole tokens, so
// only then can a filter select a class. Asserted on the KEY, what the broker
// matches on, since a correct listing proves nothing about the filter.
func TestAResourceClassIsItsOwnSubjectToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		resource string
		want     string
	}{
		{"seat:alice", "seat.alice"},
		{"node:node-0", "node.node-0"},
		{"worker:scheduler", "worker.scheduler"},
		// A name carrying the separator is MORE segments; the class is
		// still the leading one.
		{"merge:proj:7", "merge.proj.7"},
		// A dot in a name stays escaped, or it would add a token.
		{"seat:alice.smith", "seat.alice=2Esmith"},
	}
	for _, c := range cases {
		if got := encodeResource(c.resource); got != c.want {
			t.Errorf("encodeResource(%q) = %q, want %q", c.resource, got, c.want)
		}
	}

	// A class's filter selects its own keys alone, even where one class
	// name is a string prefix of another: a wildcard matches per TOKEN, so
	// `node.>` does not take `node-pool.x`.
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
// because the assertion is about the BROKER, which the client's own helper
// promises nothing about.
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

// THE BROKER NARROWS THE CLASS READS, measured rather than trusted.
//
// Both reads run on the sweep's ticker: membership, and the placement hints
// over the epochs bucket, which has NO TTL and so holds every resource ever
// leased. The count is taken INSIDE the walk: a filter that did not narrow
// yields the same rows and moves everything else over the wire.
func TestAClassReadMovesOnlyItsOwnClass(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	s := openStore(t, nc, time.Minute)
	ctx := context.Background()

	for _, r := range []string{"seat:ceo", "seat:eng", "seat:ops", "worker:scheduler", "move:task-1"} {
		if _, _, err := s.TryAcquire(ctx, r, coord.AcquireOptions{
			Owner: "node-a", TTL: time.Minute, Preferred: "node-a",
		}); err != nil {
			t.Fatalf("claim %s: %v", r, err)
		}
	}
	if _, _, err := s.TryAcquire(ctx, coord.NodeResource("node-a"), coord.AcquireOptions{
		Owner: "node-a:1", TTL: time.Minute, Ungated: true,
	}); err != nil {
		t.Fatalf("claim presence: %v", err)
	}

	count := func(kv jetstream.KeyValue, class coord.Class) int {
		t.Helper()
		n := 0
		if err := s.eachUnder(ctx, kv, class, "the "+string(class)+" records",
			func(jetstream.KeyValueEntry) error { n++; return nil }); err != nil {
			t.Fatalf("walk %s: %v", class, err)
		}
		return n
	}

	// Six resources across four classes: seats and presence in the seat
	// lease bucket, the duty and the walk claim in the duty bucket. The
	// epochs bucket holds all six, keeping a counter monotonic across a move
	// between lease buckets.
	for _, c := range []struct {
		lane  *lane
		class coord.Class
		want  int
	}{
		{s.leases, coord.ClassSeat, 3},
		{s.leases, coord.ClassNode, 1},
		{s.duties, coord.ClassWorker, 1},
		{s.duties, coord.Class("move"), 1},
	} {
		if got := count(c.lane.kv, c.class); got != c.want {
			t.Errorf("the %s lease walk was handed %d records for the %d it wanted; "+
				"the broker is not filtering and the membership read is moving "+
				"every seat in the fleet", c.class, got, c.want)
		}
		if got := count(s.counter.kv, c.class); got != c.want {
			t.Errorf("the %s epoch walk was handed %d records for the %d it wanted; "+
				"that bucket has no TTL, so an unnarrowed read here grows with "+
				"the deployment's whole history", c.class, got, c.want)
		}
	}

	// THE SEAT LEASE BUCKET CARRIES ONLY SEATS AND PRESENCE, the other half
	// of a cheap membership read and of the gate's view of it.
	for _, class := range []coord.Class{coord.ClassWorker, coord.Class("move")} {
		if got := count(s.leases.kv, class); got != 0 {
			t.Errorf("the seat lease bucket holds %d %s records; this build writes "+
				"every lease outside seats and presence to the duty bucket", got, class)
		}
	}

	// And the answers are still right.
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
// Rewriting would let the node that booted LAST redefine every other node's
// lease life; believing the configured TTL would let validateTTL accept claims
// the bucket will not honour. See [Open], "What adopting costs".
func TestASecondOpenAdoptsTheLeaseTTLInForce(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("t%d", bucketSeq.Add(1))

	const inForce = 90 * time.Second
	first, err := Open(context.Background(), jsOf(nc), jsOf(nc), Config{TTL: inForce, BucketPrefix: prefix, Reach: InProcess})
	if err != nil {
		t.Fatalf("the first Open: %v", err)
	}
	if first.TTL() != inForce {
		t.Fatalf("the node that created the bucket reports %v, want %v",
			first.TTL(), inForce)
	}

	// THE SECOND NODE ASKS FOR SOMETHING ELSE, as nodes with differing
	// Tier A files do.
	second, err := Open(context.Background(), jsOf(nc), jsOf(nc), Config{TTL: 30 * time.Second, BucketPrefix: prefix, Reach: InProcess})
	if err != nil {
		t.Fatalf("a second Open against an existing bucket: %v", err)
	}
	if second.TTL() != inForce {
		t.Errorf("the second node reports a lease TTL of %v; the bucket's is "+
			"%v, and a store that believes its own config here hands back "+
			"deadlines the bucket will not honour", second.TTL(), inForce)
	}

	// AND THE BUCKET ITSELF IS UNCHANGED — the second node wrote nothing.
	status, err := second.leases.kv.Status(context.Background())
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
// Fatal where a TTL difference is only reported, because it has no symptom
// until a node is lost, when the leases, fencing epochs and secrets turn out
// to have been on one disk. Raising stream.replicas on a running fleet adopts
// the old buckets, and nothing rewrites a bucket that exists ([openBucket]),
// so the only honest move is to refuse.
func TestABucketReplicatedBelowThisNodesConfigIsRefused(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("t%d", bucketSeq.Add(1))

	// THE FLEET STARTED AT ONE REPLICA, as a single node or early cluster does.
	if _, err := Open(context.Background(), jsOf(nc), jsOf(nc), Config{TTL: 45 * time.Second, BucketPrefix: prefix, Replicas: 1, Reach: InProcess}); err != nil {
		t.Fatalf("the first Open: %v", err)
	}

	// AND THE OPERATOR RAISED IT; the buckets are still single-replica.
	_, err := Open(context.Background(), jsOf(nc), jsOf(nc), Config{TTL: 45 * time.Second, BucketPrefix: prefix, Replicas: 3, Reach: InProcess})
	if err == nil {
		t.Fatal("a node configured for 3 replicas adopted single-replica " +
			"coordination and reported itself healthy — the leases, the " +
			"fencing epochs and the company's secrets are on one disk and " +
			"nothing says so")
	}
	// THE COUNTS ARE IN THE MESSAGE, so an operator knows which side to
	// change, and THE FIELD IS stream.replicas: the coordination buckets
	// take their count from it, and there is no coordination.replicas.
	for _, want := range []string{"1x", "3x", "stream.replicas"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	// AND A BUCKET AT OR ABOVE THE CONFIG STILL OPENS, so a single-replica
	// development node is not locked out of a replicated fleet.
	if _, err := Open(context.Background(), jsOf(nc), jsOf(nc), Config{TTL: 45 * time.Second, BucketPrefix: prefix, Replicas: 1, Reach: InProcess}); err != nil {
		t.Errorf("a node configured for fewer replicas than the bucket has "+
			"was refused: %v", err)
	}
}

// AN ADMITTED CHARGE CLEARS ONLY THE REFUSAL IT SAW.
//
// An interleaving the contract suite cannot reach: a charge admitted under a
// refusal clears it only after another caller stamped a newer one. A clear
// ignoring which stamp it saw would hide a scope refusing right now.
func TestAnAdmittedChargeLeavesANewerRefusalStanding(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	ctx := t.Context()
	w := testWindows()
	stamp := func() coord.Tally {
		t.Helper()
		tally, err := store.tally(ctx, coord.OrgScope, w)
		if err != nil {
			t.Fatalf("tally: %v", err)
		}
		return tally
	}
	refuse := func() {
		t.Helper()
		got, err := store.Charge(ctx, coord.ChargeRequest{
			Seat: coord.AgentScope("x"), Tokens: 10, Windows: w, OrgCaps: coord.Caps{period.Day: 5},
		})
		if err != nil || got.OK {
			t.Fatalf("Charge = (%+v, %v), want the company refusing", got, err)
		}
	}

	refuse()
	seen := stamp()
	// A distinct instant, so the two stamps cannot share a clock tick.
	time.Sleep(2 * time.Millisecond)
	refuse()
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
	return spentBy(t, store, coord.OrgScope)
}

// spentBy is one scope's day, week and month spend at testWindows.
func spentBy(t *testing.T, store *FleetStore, scope string) [3]int {
	t.Helper()
	u, err := store.Used(t.Context(), scope, testWindows())
	if err != nil {
		t.Fatalf("Used(%s): %v", scope, err)
	}
	return [3]int{u.Windows[0].Used, u.Windows[1].Used, u.Windows[2].Used}
}

// AN UNREACHABLE COUNTER IS AN ERROR, NEVER A REFUSAL.
//
// "Out of tokens" is a budget event; "the counter could not be read" is an
// outage. A caller that cannot tell them apart stops a healthy company over a
// blip or spends past a cap it could not read. A dead context stands in for
// the unreachable store.
func TestAnUnreachableCounterIsAnErrorNeverARefusal(t *testing.T) {
	t.Parallel()
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

// A CHARGE WHOSE SEAT WRITE FAILS KEEPS THE ROUND ON THE COMPANY.
//
// A charge is handed tokens already spent, and the company is written before
// the seat, so when the seat's write fails the company's record is true and
// stays: the answer is an error naming the partial. Taking the company's half
// back would leave the round on neither counter, since nothing that charges
// asks again.
func TestAChargeWhoseSeatWriteFailsKeepsTheRoundOnTheCompany(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	seat := coord.AgentScope("x")
	store.budgets = failWriting{KeyValue: store.budgets, key: encodeKey(seat)}

	got, err := store.Charge(t.Context(), coord.ChargeRequest{
		Seat: seat, Tokens: 10, Windows: testWindows(),
		OrgCaps: coord.Caps{period.Day: 100}, SeatCaps: coord.Caps{period.Day: 100},
	})
	var partial *coord.SeatUncountedError
	if !errors.As(err, &partial) || partial.Seat != seat || partial.Tokens != 10 {
		t.Fatalf("Charge = (%+v, %v), want an error naming the 10 tokens the seat "+
			"%s is missing", got, err, seat)
	}
	if got.OK || got.RefusedScope != "" {
		t.Fatalf("Charge = %+v: a failed write reported as a decision", got)
	}
	if used := orgSpent(t, store); used != [3]int{10, 10, 10} {
		t.Errorf("org day/week/month = %v, want the 10 the round was billed in every "+
			"window: the company's record of a spent round was taken back", used)
	}
	if used := spentBy(t, store, seat); used != [3]int{0, 0, 0} {
		t.Errorf("the seat's day/week/month = %v, want nothing: the fault did not stop "+
			"its write", used)
	}
}

// THE SEAT'S HALF OUTLIVES A CALLER WHO HANGS UP AFTER THE COMPANY'S.
//
// Once the company's write lands the round is spent whatever the caller does,
// and a caller hanging up then (a turn cancelled, a node draining) is
// ordinary. On the caller's context the seat's write failed and its ceiling
// undercounted for the rest of the window. So every charge or post-charge
// reaching the seat after the company's write finishes on a context of its
// own, and answers what it decided.
func TestTheSeatsHalfOutlivesACallerWhoHangsUp(t *testing.T) {
	t.Parallel()
	seat := coord.AgentScope("x")
	charge := func(orgCap, seatCap int) func(context.Context, *FleetStore) (coord.Spend, error) {
		return func(ctx context.Context, store *FleetStore) (coord.Spend, error) {
			return store.Charge(ctx, coord.ChargeRequest{
				Seat: seat, Tokens: 10, Windows: testWindows(),
				OrgCaps:  coord.Caps{period.Day: orgCap},
				SeatCaps: coord.Caps{period.Day: seatCap},
			})
		}
	}
	for _, tc := range []struct {
		name string
		call func(context.Context, *FleetStore) (coord.Spend, error)
		want func(coord.Spend) bool
	}{
		{"a charge the company refuses", charge(5, 100), func(got coord.Spend) bool {
			return !got.OK && got.RefusedScope == "org" && got.RefusedUsed == 10
		}},
		{"a charge the seat refuses", charge(100, 5), func(got coord.Spend) bool {
			return !got.OK && got.RefusedScope == "agent" && got.RefusedUsed == 10
		}},
		{"a charge both admit", charge(100, 100), func(got coord.Spend) bool {
			return got.OK && got.Agent.In(period.Day).Used == 10
		}},
		{"a post-charge", func(ctx context.Context, store *FleetStore) (coord.Spend, error) {
			return store.PostCharge(ctx, seat, 10, testWindows())
		}, func(got coord.Spend) bool {
			return got.OK && got.Agent.In(period.Day).Used == 10
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := openFleet(t, embeddedNATS(t))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store.budgets = hangUpAfterWriting{
				KeyValue: store.budgets, key: encodeKey(coord.OrgScope), hangUp: cancel,
			}

			got, err := tc.call(ctx, store)
			if err != nil || !tc.want(got) {
				t.Fatalf("answer = (%+v, %v), want what the counters decided: a caller "+
					"hanging up after the company's write is not an outage", got, err)
			}
			if ctx.Err() == nil {
				t.Fatal("the caller never hung up: the fault was not injected")
			}
			for _, scope := range []string{coord.OrgScope, seat} {
				if used := spentBy(t, store, scope); used != [3]int{10, 10, 10} {
					t.Errorf("%s day/week/month = %v, want the 10 the round spent in "+
						"every window: its write ran on the caller's cancelled context",
						scope, used)
				}
			}
		})
	}
}

// A POST-CHARGE WHOSE SEAT WRITE FAILS KEEPS ITS SPEND ON THE COMPANY, AND THE
// SEAT'S HALF CAN BE FINISHED ALONE.
//
// The spend already happened (an executor launch collected, an auxiliary
// call), so the company's record is true when the seat's write then fails.
// Taking it back would lose the spend for good: an auxiliary call is never
// offered again, an executor launch only when its resume fails. Kept, the
// partial is named, and a caller that offers the spend again finishes the
// seat's half alone.
func TestAPostChargeWhoseSeatWriteFailsKeepsTheCompanysHalf(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	seat := coord.AgentScope("x")
	healthy := store.budgets
	store.budgets = failWriting{KeyValue: healthy, key: encodeKey(seat)}

	got, err := store.PostCharge(t.Context(), seat, 10, testWindows())
	var partial *coord.SeatUncountedError
	if !errors.As(err, &partial) || partial.Seat != seat || partial.Tokens != 10 {
		t.Fatalf("PostCharge = (%+v, %v), want an error naming the 10 tokens the seat "+
			"%s is missing", got, err, seat)
	}
	if used := orgSpent(t, store); used != [3]int{10, 10, 10} {
		t.Errorf("org day/week/month = %v after the seat's write failed, want the 10 "+
			"the company was billed: its half was taken back", used)
	}

	// The seat's half, finished alone: the company is not counted twice.
	store.budgets = healthy
	u, err := store.PostChargeSeat(t.Context(), seat, partial.Tokens, testWindows())
	if err != nil {
		t.Fatalf("PostChargeSeat: %v", err)
	}
	if day := u.Windows[0].Used; day != 10 {
		t.Errorf("the seat's day = %d after its half was finished, want 10", day)
	}
	if used := orgSpent(t, store); used != [3]int{10, 10, 10} {
		t.Errorf("org day/week/month = %v after the seat's half was finished, want "+
			"still 10: finishing the seat counted the company again", used)
	}
}

// AN ADMITTED CHARGE CLEARS THE STAMP EVEN IF THE CALLER HAS HUNG UP.
//
// The clear runs after BOTH counters are written, when the charge is a fact.
// On the caller's context a hang-up failed it, and the scope went on telling
// every dashboard it was refusing while it had just admitted a charge.
func TestACancelledChargeStillClearsTheRefusalItAdmittedPast(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	seat := coord.AgentScope("x")
	w := testWindows()
	// A refusal on each scope at a ceiling of 5: the company's, then the
	// seat's under a roomy company ceiling.
	for _, caps := range []struct{ org, seat coord.Caps }{
		{coord.Caps{period.Day: 5}, nil},
		{coord.Caps{period.Day: 100}, coord.Caps{period.Day: 5}},
	} {
		if got, err := store.Charge(t.Context(), coord.ChargeRequest{
			Seat: seat, Tokens: 10, Windows: w, OrgCaps: caps.org, SeatCaps: caps.seat,
		}); err != nil || got.OK {
			t.Fatalf("setup Charge = (%+v, %v), want a refusal", got, err)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The SEAT's write is the last before the clear, so a hang-up there
	// leaves both counters written and the clear to make.
	store.budgets = hangUpAfterWriting{
		KeyValue: store.budgets, key: encodeKey(seat), hangUp: cancel,
	}

	// The ceilings raised, giving the refusing scopes room again.
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
// A counter uncharged past the bucket's retention ages out, so a seat that
// stopped running keeps no record for the deployment's life. The bucket's
// retention here is short enough to watch.
func TestAnUnchargedCounterAgesOut(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	store := openFleetWithTTL(t, nc, 500*time.Millisecond)
	// BOUNDED, so a listing that never ends fails this case by name, not
	// the package's timeout: counters aging out between a consumer's creation
	// and its first delivery once left one waiting on an end marker
	// ([passOver]).
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
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

// THE COUNTER RECORD'S WIRE IS SHARED BY EVERY BUILD ON THE BUCKET, and it
// fails open: an unknown slot key decodes as an unlabelled slot every charge
// rolls, so a renamed key hands each scope its allowance back. Pinned as
// bytes, both ways.
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

// openFleet opens a fleet store on its own buckets, for a test that reaches
// inside one.
func openFleet(t *testing.T, nc *nats.Conn) *FleetStore {
	t.Helper()
	store, err := OpenFleet(context.Background(), jsOf(nc), FleetConfig{
		Reach:         InProcess,
		MaxRateWindow: time.Minute, MaxClaimTTL: time.Minute,
		LedgerRetention: time.Minute, FireRetention: time.Minute,
		FollowRetention: time.Minute, RebaseRetention: time.Minute,
		StatusFreshness:  time.Minute,
		CustodyRetention: time.Minute,
		BudgetRetention:  time.Minute,
		BucketPrefix:     fmt.Sprintf("f%d", bucketSeq.Add(1)),
	})
	if err != nil {
		t.Fatalf("OpenFleet: %v", err)
	}
	return store
}

// hangUpAfterWriting is the budgets bucket with one fault: the moment key is
// written, the caller's context is cancelled.
type hangUpAfterWriting struct {
	jetstream.KeyValue
	key    string
	hangUp context.CancelFunc
}

func (k hangUpAfterWriting) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	rev, err := k.KeyValue.Create(ctx, key, value, opts...)
	if err == nil && key == k.key {
		k.hangUp()
	}
	return rev, err
}

func (k hangUpAfterWriting) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	rev, err := k.KeyValue.Update(ctx, key, value, revision)
	if err == nil && key == k.key {
		k.hangUp()
	}
	return rev, err
}

// failWriting is a counter bucket refusing every write to key with an error
// that is not a lost race, so the write fails once rather than being retried.
type failWriting struct {
	jetstream.KeyValue
	key string
}

var errWriteRefused = errors.New("nats: the broker refused the write (injected)")

func (k failWriting) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if key == k.key {
		return 0, errWriteRefused
	}
	return k.KeyValue.Create(ctx, key, value, opts...)
}

func (k failWriting) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if key == k.key {
		return 0, errWriteRefused
	}
	return k.KeyValue.Update(ctx, key, value, revision)
}
