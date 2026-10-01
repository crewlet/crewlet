package jetstreamtest

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// A NODE'S CONNECTION STAYS ON ITS OWN MEMBER, AND DOES NOT OUTLIVE IT.
//
// Each node of an embedded fleet talks to the member in its own process. A
// member advertises its peers' client URLs, and a connection that takes them
// into its reconnect pool goes to a peer the moment it drops: for the rest of
// its life on a slow-consumer drop, and for good when its own member stops —
// where the node, instead of running its reconnects out and stopping for its
// supervisor to bring the member back, ran on through a peer with its member
// dead, the cluster a replica short and nothing saying so.
//
// The CONTROL is a connection with the client's own defaults to the same
// member: it reads the peers, and when the member stops it moves to one — so
// the case is about the dial a node's queue takes, never about a cluster that
// advertises nobody or a peer nothing could have reached.
//
// Mutation: drop IgnoreDiscoveredServers from the embedded dial and both the
// pool row and the stop row go red.
func TestANodesConnectionStaysOnItsOwnMember(t *testing.T) {
	t.Parallel()
	c := StartCluster(t, 3, js.Config{})
	q := c.Client(t, 0)
	own := q.Conn().ConnectedUrl()

	control, err := nats.Connect(own, nats.MaxReconnects(-1),
		nats.ReconnectWait(100*time.Millisecond))
	if err != nil {
		t.Fatalf("the control connection: %v", err)
	}
	t.Cleanup(control.Close)
	within(t, 30*time.Second, "the member to advertise its peers to a client",
		func() bool { return len(control.DiscoveredServers()) > 0 })

	if pool := q.Conn().Servers(); len(pool) != 1 || pool[0] != own {
		t.Errorf("the node's connection would reconnect to %v, want its own "+
			"member %s alone", pool, own)
	}

	c.Servers[0].Shutdown()
	within(t, 30*time.Second, "the control connection to move to a peer",
		func() bool { return control.IsConnected() && control.ConnectedUrl() != own })

	// LONGER THAN THE NODE'S OWN RECONNECT WAIT, several times over: a dial
	// that took the peers moves within one wait of the control, and one that
	// did not stays down for its member's whole reconnect span.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if q.Conn().IsConnected() {
			t.Fatalf("the node's connection is on %s, a peer, with its own "+
				"member stopped: it would never run its reconnects out",
				q.Conn().ConnectedUrl())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// within waits for cond, failing the case naming what it waited for.
func within(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s", budget, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
