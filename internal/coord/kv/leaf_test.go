package kv

import (
	"fmt"
	"net"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/jsapi"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
)

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
func TestFleetContractThroughALeaf(t *testing.T) {
	t.Parallel()
	client := leafClient(t)
	coordtest.RunFleet(t, func(t *testing.T) coord.Fleet {
		return openFleetVia(t, client, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	})
}

// leafClient starts a member serving leaves and a leaf joined to it, and is a
// JetStream client of the LEAF — in the embedded fleet's domain, the only API
// a leaf's broker carries across its link.
func leafClient(t *testing.T) jetstream.JetStream {
	t.Helper()
	port := leafMember(t)
	leaf, err := js.StartServer(t.Context(), js.Config{ServerName: "leaf",
		LeafURLs: []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", port)}})
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
// listener holds, trying again on another port when a start fails.
//
// # Why a retry rather than one attempt
//
// The port is a loopback port nothing held a moment ago ([unusedPort]), and
// between that moment and the member binding it something else can take it —
// in this binary, most likely one of the cluster cases running beside these,
// which reserve their route ports the same way. The member's own probe
// ([js.PortAvailable], run on its configured leaf port before the server is
// asked for it) reports that loss at once as [js.ErrRoutePortTaken]; one lost
// inside the start itself surfaces as a readiness wait that ran out. Either
// way a fresh number is the remedy, which is the jetstreamtest harness's rule
// for the same race one layer up — "what DOES fix it is noticing, and trying
// again with different numbers" — and so is the default of retrying whatever
// failed: a start that fails the same way every time costs a few attempts and
// is then reported with its own error, where a race read as fatal ends the
// test for a reason that is nobody's bug.
//
// [jetstreamtest.ClusterStartAttempts] attempts, the harness's own number for
// that race, rather than a second spelling of it.
func leafMember(t *testing.T) int {
	t.Helper()
	var last error
	for attempt := 1; attempt <= jetstreamtest.ClusterStartAttempts; attempt++ {
		port := unusedPort(t)
		// A MEMBER THAT SERVES LEAVES PERSISTS, or it is refused.
		member, err := js.StartServer(t.Context(), js.Config{ServerName: "member",
			LeafHost: "127.0.0.1", LeafPort: port, StoreDir: t.TempDir()})
		if err == nil {
			t.Cleanup(member.Shutdown)
			return port
		}
		last = err
		t.Logf("leaf member attempt %d/%d on port %d failed, retrying on "+
			"another port: %v", attempt, jetstreamtest.ClusterStartAttempts, port, err)
	}
	t.Fatalf("no member serving leaves started in %d attempts: %v",
		jetstreamtest.ClusterStartAttempts, last)
	return 0
}

// unusedPort is a loopback port nothing held a moment ago. The race to bind
// it is the test's to lose, and [leafMember] tries again when it does.
func unusedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
