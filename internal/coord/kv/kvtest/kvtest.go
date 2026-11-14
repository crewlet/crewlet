// Package kvtest stands up one company's coordination store as two nodes reach
// it: a member, in its own process, and a node without `data`, joined to that
// member as a leaf across a link that carries what the leaf sends, and what it
// is sent back, slowly (internal/queue/jetstream/linktest). A case changes a
// value through the leaf to show what a change waits for when its read's reply
// and its write's bytes take a while to cross, and sets up and reads back
// through the member, whose own connection crosses nothing, so its setup costs
// none of the link's time and its reads are never held behind a change still
// crossing ([Across.Holds]).
//
// ONE HELPER FOR EVERY PACKAGE whose writes of the store cross a leaf's link —
// the sandbox's run rows, and the engine's writes of them — so the cases share
// one topology rather than each standing up its own copy of it.
// internal/coord/kv's own cases keep theirs, since a test of package kv cannot
// import a package that imports it.
package kvtest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// Across is one company's coordination buckets as two nodes reach them.
type Across struct {
	// Member is the store over the member's own connection, in its own
	// process: what a case sets up and reads back through.
	Member *kv.FleetStore

	// Leaf is the store over a leaf joined to that member across the slow
	// link, opened as the engine opens a node's store across a link
	// ([kv.AcrossLink]): what a case writes through.
	Leaf *kv.FleetStore
}

// SlowLeaf starts a member serving leaves, a link in front of its leaf
// listener carrying rate bytes a second each way ([linktest.SlowEachWay]), and
// a leaf joined to the member across that link, and opens the fleet store on
// each: one company's buckets under one prefix, the member's reached in its
// own process and the leaf's across its link. Everything ends with the case.
func SlowLeaf(t testing.TB, rate int64) Across {
	t.Helper()
	// A MEMBER THAT SERVES LEAVES PERSISTS, or it is refused; and every
	// server a test builds declares its size and keeps its store in a
	// directory of the case's (brokertest).
	member, err := js.StartServer(t.Context(), js.Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: js.AnyPort, StoreDir: brokertest.TempDir(t),
		StoreMaxBytes: brokertest.StoreMaxBytes, FleetAuth: fleetAuth()})
	if err != nil {
		t.Fatalf("start a member serving leaves: %v", err)
	}
	t.Cleanup(member.Shutdown)
	link := linktest.SlowEachWay(t, fmt.Sprintf("127.0.0.1:%d", member.LeafPort()), rate)
	leaf, err := js.StartServer(t.Context(), js.Config{ServerName: "leaf",
		LeafURLs: []string{"nats-leaf://" + link.Addr()}, FleetAuth: fleetAuth()})
	if err != nil {
		t.Fatalf("start the leaf: %v", err)
	}
	t.Cleanup(leaf.Shutdown)
	// THE MEMBER'S FIRST, which creates the buckets; the leaf's adopts
	// them, asked across its link once it is up.
	return Across{
		Member: open(t, client(t, "member", member), kv.InProcess),
		Leaf:   open(t, client(t, "leaf", leaf), kv.AcrossLink),
	}
}

// client is a JetStream client of server's own connection, in the embedded
// fleet's domain: the one API a leaf's broker carries across its link, and
// the member's own. name says which server, for a failure.
func client(t testing.TB, name string, server *js.Server) jetstream.JetStream {
	t.Helper()
	nc, err := server.Conn()
	if err != nil {
		t.Fatalf("connect to the %s: %v", name, err)
	}
	t.Cleanup(nc.Close)
	c, err := jsapi.Embedded().Client(nc)
	if err != nil {
		t.Fatalf("a client of the fleet's domain: %v", err)
	}
	return c
}

// open opens the fleet store over c, reaching its buckets as reach says.
//
// The retentions are a case's, not production's: each is above the broker's
// 100 ms floor and far longer than a case takes, so nothing lapses under a
// case that did not ask it to.
func open(t testing.TB, c jetstream.JetStream, reach kv.Reach) *kv.FleetStore {
	t.Helper()
	store, err := kv.OpenFleet(context.Background(), c, kv.FleetConfig{
		Reach:            reach,
		BucketPrefix:     "slowleaf",
		MaxRateWindow:    time.Minute,
		MaxClaimTTL:      10 * time.Minute,
		LedgerRetention:  10 * time.Minute,
		FireRetention:    10 * time.Minute,
		FollowRetention:  10 * time.Minute,
		RebaseRetention:  10 * time.Minute,
		BudgetRetention:  time.Hour,
		StatusFreshness:  10 * time.Minute,
		CustodyRetention: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("open the fleet store reaching its buckets %s: %v", reach, err)
	}
	return store
}

// Holds is what the member holds of turnID's run now — its record, and
// whether it has one — read ONCE: the first state of a watch of the run's key
// on the member's own connection ([kv.FleetStore.WatchSandboxRun]), which a
// consumer delivers whole and raw, certified against the bucket's key index.
// Asked for as long as the broker's longest silence (jsprovision's
// LookupBudget), each of the watch's requests answered within it by a broker
// that answers at all.
//
// NOT THE LEADER'S READ a change of the run makes. On a member's own
// connection that read keeps the read term, asked again every two seconds
// while nobody answers it, since a writer waits on it (internal/coord/kv's
// holdingLarge); and its reply carries a row at its bound base64-encoded in
// eight mebibytes, which a runner loaded and under the race detector takes
// seconds to build and decode. Read back so, a case's row had another whole
// reply built beside the first at every re-ask, and could run out the lookup
// ceiling on the load the case is not about. A watch's first state is one
// delivery of the record as it is.
func (a Across) Holds(t testing.TB, turnID string) (coord.Record, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), jsprovision.LookupBudget)
	defer cancel()
	updates, err := a.Member.WatchSandboxRun(ctx, turnID)
	if err != nil {
		t.Fatalf("read run %s back through the member: %v", turnID, err)
	}
	first, delivered := <-updates
	if !delivered {
		t.Fatalf("read run %s back through the member: the watch ended before its first state: %v",
			turnID, context.Cause(ctx))
	}
	return first.Record, first.Found
}

// fleetAuth is the keyring a member serving leaves and every leaf joined to
// it hold: a leaf link is admitted only on a key its member holds. A literal
// placeholder, never a key anybody deploys.
var fleetAuth = sync.OnceValue(func() js.FleetAuth {
	auth, err := js.FleetAuthFrom(runtoken.OneKey("kvtest-leaf",
		"the slow leaf cases' placeholder key material"))
	if err != nil {
		panic(err)
	}
	return auth
})

// PadKey is the key [AtTheBound] adds to a value: one no build of the engine
// declares, so a reader that carries what it does not know writes it back
// whole, as it would a key a later build added.
const PadKey = "kvtest_pad"

// AtTheBound is object — a JSON object — with [PadKey] added, its words random
// text, so that it weighs room bytes short of what one key of the store holds
// ([coord.MaxValueBytes]): a value at its bound, less the room a write of it
// adds. Random, so the bytes on the link are the value's own and not what a
// compressing link would leave of them.
func AtTheBound(t testing.TB, object []byte, room int) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(object, &fields); err != nil {
		t.Fatalf("the value to pad is not a JSON object: %v", err)
	}
	if _, taken := fields[PadKey]; taken {
		t.Fatalf("the value to pad already carries %q", PadKey)
	}
	fields[PadKey] = json.RawMessage(`""`)
	bare, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode the value: %v", err)
	}
	fill := coord.MaxValueBytes - room - len(bare)
	if fill <= 0 {
		t.Fatalf("the value is %d bytes, which leaves no room to pad it to %d short of %d",
			len(bare), room, coord.MaxValueBytes)
	}
	// BASE64 OF RANDOM BYTES, which JSON writes as it is: no character of
	// its alphabet is escaped, so the padding's length is its encoding's.
	random := make([]byte, base64.StdEncoding.DecodedLen(fill)+3)
	if _, err = rand.Read(random); err != nil {
		t.Fatalf("random padding: %v", err)
	}
	fields[PadKey], err = json.Marshal(base64.StdEncoding.EncodeToString(random)[:fill])
	if err != nil {
		t.Fatalf("encode the padding: %v", err)
	}
	padded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode the padded value: %v", err)
	}
	if want := coord.MaxValueBytes - room; len(padded) != want {
		t.Fatalf("the padded value is %d bytes, want %d", len(padded), want)
	}
	return padded
}
