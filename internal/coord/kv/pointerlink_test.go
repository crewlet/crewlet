package kv

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// pointerLinkRate is the pace the leaf cases below hold a link to, each way:
// 160 KiB a second, about five times the slowest a link is allowed
// ([objstore.MiBPace]), so a body at its bound takes about fifty-one seconds to
// cross back as a read's reply: past a probe's budget and the flat read term,
// far inside what it is given at the slowest pace.
const pointerLinkRate = 160 << 10

// probeBudget is internal/engine's ProbeReadBudget, an eighth of the reconcile
// interval: what every probe and heartbeat gives its read of the pointer.
const probeBudget = 1875 * time.Millisecond

// pointerOrigin is the origin the cases' activations carry.
var pointerOrigin = coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorOperator,
	Source: "api", CreatedAt: time.Date(2026, 9, 2, 7, 0, 0, 0, time.UTC)}

// overASlowLeaf is one company's fleet store as a member reaches it in its
// own process and a leaf across a link slow both ways ([pointerLinkRate]).
func overASlowLeaf(t *testing.T) (member, leaf *FleetStore) {
	t.Helper()
	// A MEMBER THAT SERVES LEAVES PERSISTS, or it is refused.
	srv, err := js.StartServer(t.Context(), js.Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: js.AnyPort, StoreDir: brokertest.TempDir(t),
		StoreMaxBytes: brokertest.StoreMaxBytes, FleetAuth: leafFleetAuth()})
	if err != nil {
		t.Fatalf("start a member serving leaves: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	link := linktest.SlowEachWay(t, fmt.Sprintf("127.0.0.1:%d", srv.LeafPort()), pointerLinkRate)
	nc, err := srv.Conn()
	if err != nil {
		t.Fatalf("connect to the member: %v", err)
	}
	t.Cleanup(nc.Close)
	client, err := jsapi.Embedded().Client(nc)
	if err != nil {
		t.Fatalf("a client of the member: %v", err)
	}
	// THE MEMBER'S FIRST, which creates the buckets; the leaf's adopts them.
	prefix := fmt.Sprintf("x%d", bucketSeq.Add(1))
	member = openFleetVia(t, client, InProcess, prefix)
	leaf = openFleetVia(t, leafClientAt(t, link.Addr(), "leaf"), AcrossLink, prefix)
	return member, leaf
}

// activateAtTheBound activates, through store, a body of exactly what one key
// holds: random text, so the bytes on the link are its own.
func activateAtTheBound(t *testing.T, store *FleetStore) coord.Activation {
	t.Helper()
	const frame = len(`{"v":""}`)
	random := make([]byte, base64.StdEncoding.DecodedLen(coord.MaxValueBytes-frame)+3)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	text := base64.StdEncoding.EncodeToString(random)[:coord.MaxValueBytes-frame]
	published, err := store.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-1", Payload: []byte(`{"v":"` + text + `"}`), At: time.Now(),
		Origin: pointerOrigin})
	if err != nil {
		t.Fatalf("activate a body at its bound: %v", err)
	}
	if published.Body.Bytes != coord.MaxValueBytes {
		t.Fatalf("the body is %d bytes, want %d", published.Body.Bytes, coord.MaxValueBytes)
	}
	return published
}

// A POINTER READ ACROSS A LEAF'S LINK CARRIES NO BODY. Every probe, heartbeat
// and tick on every node reads the pointer, a probe inside 1.875 s. With the
// sealed document in it, a company at its bound made every such read on a
// leaf an eight-mebibyte reply, fifty-one seconds here, and the probe failed;
// the pointer alone crosses in milliseconds.
//
// Mutation: carry the payload in the pointer again, and the read misses the
// probe's budget.
func TestAPointerReadAcrossALeafsLinkCarriesNoBody(t *testing.T) {
	t.Parallel()
	member, leaf := overASlowLeaf(t)
	published := activateAtTheBound(t, member)
	ctx, cancel := context.WithTimeout(t.Context(), probeBudget)
	defer cancel()
	target, found, err := leaf.Target(ctx)
	if err != nil || !found {
		t.Fatalf("the pointer read across the leaf's link inside %v: found=%v err=%v", probeBudget, found, err)
	}
	if target.Epoch != published.Epoch || target.Body != published.Body {
		t.Errorf("the leaf read %+v, want the activation published, %+v", target, published)
	}
}

// A BODY AT ITS BOUND IS READ ACROSS A LEAF'S LINK, WITH NO DEADLINE OF ITS
// CALLER'S: its read is given the arrival of the size the pointer records, so
// its reply of eight mebibytes, fifty-one seconds here, is waited for whole.
// Read at the flat read term, it was cut at five seconds every time.
//
// Mutation: read the body at the read term, or at the pointer's size, and it
// is cut before its reply arrives.
func TestABodyAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk(t *testing.T) {
	t.Parallel()
	member, leaf := overASlowLeaf(t)
	published := activateAtTheBound(t, member)
	target, found, err := leaf.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("the pointer read across the leaf's link: found=%v err=%v", found, err)
	}
	body, found, err := leaf.Body(context.Background(), target)
	if err != nil || !found {
		t.Fatalf("the body of %d bytes read across the leaf's link: found=%v err=%v",
			target.Body.Bytes, found, err)
	}
	if !published.Body.Describes(body) {
		t.Errorf("the leaf read %d bytes that are not the body published", len(body))
	}
}

// A BODY'S READ IS GIVEN THE ARRIVAL OF ITS RECORDED SIZE, AND THE POINTER'S
// THE READ TERM. The size is the one the pointer records, so the read is asked
// once and waited for whole however large the body; the pointer beside it is
// small, so the bucket is not declared large ([holdingLarge]) and its reads
// keep the read term. BodyCrossing is the crossing alone, none in process.
// Exact, from the store's own rules.
//
// Mutation: read the body without its size, or at the pointer's, and its row
// goes red; declare the config bucket large, and the pointer's does.
func TestABodysReadIsGivenTheArrivalOfItsRecordedSize(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	for _, reach := range []Reach{InProcess, AcrossLink} {
		store := openFleetVia(t, jsOf(nc), reach, fmt.Sprintf("x%d", bucketSeq.Add(1)))
		body, err := sizedReader(store.js, store.config, coord.MaxValueBytes)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := body.leader.Term(), jsprovision.ReadTerm+
			jsapi.Arrival(coord.MaxValueBytes+kvHeaderBytes, reach.pace()); got != want {
			t.Errorf("a body at its bound %s is read at a term of %v, want %v", reach, got, want)
		}
		pointer, err := newLeaderReader(store.js, store.config)
		if err != nil {
			t.Fatal(err)
		}
		if got := pointer.leader.Term(); got != jsprovision.ReadTerm {
			t.Errorf("the pointer %s is read at a term of %v, want the read term %v", reach, got,
				jsprovision.ReadTerm)
		}
		at := coord.Activation{Body: coord.BodyRef{Bytes: coord.MaxValueBytes}}
		if got, want := store.BodyCrossing(at), jsapi.Crossing(coord.MaxValueBytes+kvHeaderBytes,
			reach.pace()); got != want {
			t.Errorf("a body at its bound %s crosses in %v, want %v", reach, got, want)
		}
	}
}

// flipDeadlines is the config bucket's handle that records the deadline of
// every compare-and-set it is handed: the flips, and nothing else.
type flipDeadlines struct {
	jetstream.KeyValue
	deadlines []time.Time
}

func (f *flipDeadlines) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if key == activationKey {
		// The zero instant where none is set, which no case expects.
		deadline, _ := ctx.Deadline()
		f.deadlines = append(f.deadlines, deadline)
	}
	return f.KeyValue.Update(ctx, key, value, revision)
}

// A FLIP LANDS WITHIN HALF THE BODY'S RETENTION OF ITS BODY'S WRITE, or not at
// all: the sweep removes a body no pointer names once it is a retention old,
// read before the pointer, so a flip later than that could name a body the
// sweep just removed. Every flip, create-only and conditional, is bounded at
// exactly the instant of the body's write plus half the retention.
//
// Mutation: drop flipBy, and the flips carry no deadline.
func TestAFlipLandsWithinHalfTheBodysRetentionOfItsWrite(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	written := time.Now().Add(time.Hour).Truncate(time.Second)
	store.now = func() time.Time { return written }
	seen := &flipDeadlines{KeyValue: store.config}
	store.config = seen
	if _, err := store.Activate(t.Context(), coord.ActivationRequest{RevisionID: "rev-1",
		Payload: []byte(`{"v":1}`), At: written, ExpectAbsent: true, Origin: pointerOrigin}); err != nil {
		t.Fatalf("a create-only activation: %v", err)
	}
	if _, err := store.Activate(t.Context(), coord.ActivationRequest{RevisionID: "rev-2",
		Payload: []byte(`{"v":2}`), At: written, Expect: "rev-1", Origin: pointerOrigin}); err != nil {
		t.Fatalf("a conditional activation: %v", err)
	}
	if len(seen.deadlines) != 2 {
		t.Fatalf("%d flips were seen, want 2", len(seen.deadlines))
	}
	for i, deadline := range seen.deadlines {
		if want := written.Add(coord.BodyRetention / 2); !deadline.Equal(want) {
			t.Errorf("flip %d is bounded at %v, want %v: half the body's retention after its write",
				i+1, deadline, want)
		}
	}
}

// A BODY WRITTEN AGAIN AFTER THE SWEEP'S WALK IS KEPT: a flip coming back to
// it wrote it first, so its revision moved past the one the walk saw, and the
// purge goes only through that one.
//
// Mutation: purge the body's key outright, and the body the flip will name is
// gone.
func TestABodyWrittenAgainAfterTheSweepsWalkIsKept(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	ctx := t.Context()
	first, err := store.Activate(ctx, coord.ActivationRequest{RevisionID: "rev-1",
		Payload: []byte(`{"v":1}`), At: time.Now(), Origin: pointerOrigin})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if _, err = store.Activate(ctx, coord.ActivationRequest{RevisionID: "rev-2",
		Payload: []byte(`{"v":2}`), At: time.Now(), Expect: "rev-1", Origin: pointerOrigin}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	pass, err := walkForSweep(ctx, store.js, store.config)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if _, err = store.config.Put(ctx, bodyKey(first.Body), []byte(`{"v":1}`)); err != nil {
		t.Fatalf("write rev-1's body again: %v", err)
	}
	if _, err = store.sweepBodies(ctx, pass, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, found, err := store.Body(ctx, first); err != nil || !found {
		t.Errorf("a body written again after the walk: found=%v err=%v, want kept", found, err)
	}
}
