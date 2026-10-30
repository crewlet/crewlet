package kv

import (
	"fmt"
	"sync"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/jsapi"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// leafFleetAuth is the keyring this package's members serving leaves and the
// leaves joined to them hold: every leaf link is admitted only on a key its
// member holds. A literal placeholder, never a key anybody deploys.
var leafFleetAuth = sync.OnceValue(func() js.FleetAuth {
	auth, err := js.FleetAuthFrom(runtoken.OneKey("kv-leaf",
		"the coordination store's leaf cases' placeholder key material"))
	if err != nil {
		panic(err)
	}
	return auth
})

// A LEAF'S COORDINATION STORE IS CERTIFIED ON THE SAME SUITES AS A MEMBER'S.
//
// A node without the `data` role joins the fleet as a leaf: its broker runs no
// JetStream, and every bucket its coordination store opens is a member's,
// reached across one leaf link in the fleet's domain. Its leases, its epochs,
// the activation pointer and the company's secrets are all read and written
// there — so the only way to know such a node agrees with the fleet about who
// holds what is to run every case a member's store runs against one. The
// queue's own half is internal/queue/jetstream's TestConformanceThroughALeaf.
func TestContractThroughALeaf(t *testing.T) {
	t.Parallel()
	client := leafClient(t)
	coordtest.Run(t, func(t *testing.T) coord.Backend {
		return openStoreVia(t, client, coordtest.LongTTL)
	})
}

// AND THE FLEET'S SHARED STATE: the counters, the ledgers, the claims and the
// secrets, whose every write is a compare-and-set a leaf's request has to
// reach a member's stream leader to win or lose.
//
// EVERY NODE A LEAF OF ITS OWN, joined to one member, which is where the
// counter failed: a round trip through a leaf is longer than one on a member,
// so the fixed 32 ms backoff ceiling that spread a member's writers no longer
// spread these, and the org's counter lost every one of its sixteen attempts
// to the contract's thirty-two concurrent charges under the solo half's load.
// The contention cases put [coordtest.ContendedCharges] charges through each of
// [coordtest.ContendingNodes] leaves at once, so they meet that topology.
func TestFleetContractThroughALeaf(t *testing.T) {
	t.Parallel()
	port := leafMember(t)
	clients := make([]jetstream.JetStream, coordtest.ContendingNodes)
	for i := range clients {
		clients[i] = leafClientOf(t, port, fmt.Sprintf("leaf-%d", i))
	}
	coordtest.RunFleet(t, func(t *testing.T) func() coord.Fleet {
		prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
		opened := 0
		return func() coord.Fleet {
			client := clients[opened%len(clients)]
			opened++
			return openFleetVia(t, client, prefix)
		}
	})
}

// leafClient starts a member serving leaves and a leaf joined to it, and is a
// JetStream client of the LEAF — in the embedded fleet's domain, the only API
// a leaf's broker carries across its link.
func leafClient(t *testing.T) jetstream.JetStream {
	t.Helper()
	return leafClientOf(t, leafMember(t), "leaf")
}

// leafClientOf starts a leaf named name joined to the member serving leaves on
// port, and is a JetStream client of it — one node of a fleet of leaves, each
// with a link of its own.
func leafClientOf(t testing.TB, port int, name string) jetstream.JetStream {
	t.Helper()
	leaf, err := js.StartServer(t.Context(), js.Config{ServerName: name,
		LeafURLs:  []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", port)},
		FleetAuth: leafFleetAuth()})
	if err != nil {
		t.Fatalf("start the leaf: %v", err)
	}
	t.Cleanup(leaf.Shutdown)
	nc, err := leaf.Conn()
	if err != nil {
		t.Fatalf("connect to the leaf: %v", err)
	}
	t.Cleanup(nc.Close)
	client, err := jsapi.Embedded().Client(nc)
	if err != nil {
		t.Fatalf("a client of the leaf: %v", err)
	}
	return client
}

// leafMember starts a member serving leaves and returns the port its leaf
// listener bound.
//
// ON [js.AnyPort], which the OS binds and the member names afterwards
// ([js.Server.LeafPort]), so nothing else can take the port between its
// choosing and its binding — in this binary, most likely one of the cluster
// cases running beside these. It used to reserve a loopback port nothing held
// a moment ago and name it, and to retry on another whenever the member's own
// probe found it taken: a retry for a race this way has no window for.
func leafMember(t testing.TB) int {
	t.Helper()
	// A MEMBER THAT SERVES LEAVES PERSISTS, or it is refused.
	member, err := js.StartServer(t.Context(), js.Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: js.AnyPort, StoreDir: t.TempDir(),
		FleetAuth: leafFleetAuth()})
	if err != nil {
		t.Fatalf("start a member serving leaves: %v", err)
	}
	t.Cleanup(member.Shutdown)
	return member.LeafPort()
}
