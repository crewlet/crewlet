package jetstreamtest

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// A MEMBER'S OWN CLIENT DISCOVERS NO PEER, AND DOES NOT OUTLIVE ITS MEMBER.
//
// Each node of an embedded fleet talks to the member in its own process. A
// member used to be dialled over TCP at its client listener, which bound every
// interface, and it advertised every peer's client URL — on the host's
// external address — in the INFO it sent: a connection that took them into
// its reconnect pool went to a peer the moment it dropped, for the rest of its
// life on a slow-consumer drop and for good when its own member stopped, where
// the node ran on through a peer with its member dead, the cluster a replica
// short and nothing saying so. That reconnect is held by the dial ignoring what
// it discovers; this case holds the rest: the node's connection is IN PROCESS,
// its pool is this member alone and lists nothing discovered, and the member's
// client listener — open only because routing needs it — is on loopback and
// tells NO client of any peer.
//
// The CONTROL is a client with nats's own defaults dialling member 0's client
// listener, which is what any process on the host could do: it reads every
// peer the member advertises, and before this change it read all of them.
//
// Mutations: Cluster.NoAdvertise off → the control discovers the peers, red;
// dial a clustered member over its client listener again → the in-process row,
// red; drop IgnoreDiscoveredServers and NoAdvertise together → the node's pool
// row, red.
func TestAMembersOwnClientDiscoversNoPeer(t *testing.T) {
	t.Parallel()
	c := StartCluster(t, 3, js.Config{})
	q := c.Client(t, 0)
	own := q.Conn().ConnectedUrl()

	if q.Conn().Opts.InProcessServer == nil {
		t.Fatalf("the node's connection to its own member dials %s over TCP, where "+
			"every connection to a member in this process is a pipe", own)
	}
	if pool := q.Conn().Servers(); len(pool) != 1 || pool[0] != own {
		t.Errorf("the node's connection would reconnect to %v, want its own "+
			"member %s alone", pool, own)
	}
	if found := q.Conn().DiscoveredServers(); len(found) != 0 {
		t.Errorf("the node's connection was told of %v, where a member "+
			"advertises no peer", found)
	}

	listener := c.Servers[0].ClientListener()
	if !strings.HasPrefix(listener, "127.0.0.1:") {
		t.Fatalf("member 0's client listener is %q, want loopback alone", listener)
	}
	// THE MESH IS WHOLE before the control reads anything ([StartCluster]
	// waits for every member to be routed to its peers), so the INFO the
	// control is handed on connecting already carries every peer a member
	// would advertise; the round trip after it lets any INFO sent since land.
	control, err := nats.Connect("nats://"+listener, nats.MaxReconnects(-1),
		nats.ReconnectWait(100*time.Millisecond))
	if err != nil {
		t.Fatalf("the control connection to member 0's client listener: %v", err)
	}
	t.Cleanup(control.Close)
	if err := control.Flush(); err != nil {
		t.Fatalf("the control connection's round trip: %v", err)
	}
	if found := control.DiscoveredServers(); len(found) != 0 {
		t.Errorf("a client of member 0's client listener was told of %v: a member "+
			"maps the fleet's listeners for whoever connects", found)
	}

	c.Servers[0].Shutdown()
	// LONGER THAN THE NODE'S OWN RECONNECT WAIT, several times over: a dial
	// that could reach a peer would be up there within one wait, and one that
	// cannot stays down for its member's whole reconnect span.
	//
	// ON ANOTHER MEMBER is the failure, never "connected": the client learns
	// its connection closed on its own reader goroutine, so for a moment after
	// the shutdown it still reports itself connected to its own member's URL.
	// ConnectedUrl is empty while the connection is down, so this fails
	// exactly when the connection is up somewhere it must not be.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if at := q.Conn().ConnectedUrl(); at != "" && at != own {
			t.Fatalf("the node's connection is on %s, a peer, with its own "+
				"member %s stopped: it would never run its reconnects out",
				at, own)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if q.Conn().IsConnected() {
		t.Fatalf("the node's connection is still up on %s, %v after its own "+
			"member stopped", q.Conn().ConnectedUrl(), 5*time.Second)
	}
}
