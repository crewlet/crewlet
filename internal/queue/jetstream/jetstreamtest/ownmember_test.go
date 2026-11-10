package jetstreamtest

import (
	"strings"
	"sync"
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
// listener with the operator key — the one client that listener admits, the
// nats tool on the member's host: it reads every peer the member advertises,
// and before this change any client there read all of them.
//
// AND ONCE ITS MEMBER STOPS, it lands nowhere, decided by what the client will
// reconnect to rather than by watching it for a while. Every attempt goes to a
// server in the pool — over an in-process dial, to the member in this process
// whatever the pool names — and the pool grows only by an INFO, which only a
// connected reader takes. So the pool is read once the client has said it is
// down, which it does on its way into its reconnects with its reader already
// returned: nothing can add to the pool from there, and this member alone,
// with nothing discovered, is a reconnect that cannot land on a peer. Five
// seconds of watching for a peer's URL was a bet that a reconnect which could
// land would have landed by then.
//
// Mutations: Cluster.NoAdvertise off → the control discovers the peers, red;
// dial a clustered member over its client listener again → the in-process row,
// red; drop IgnoreDiscoveredServers and NoAdvertise together → the node's pool,
// read once it is down, red; reconnect for ever → the budget row, red.
func TestAMembersOwnClientDiscoversNoPeer(t *testing.T) {
	t.Parallel()
	c := StartCluster(t, 3, js.Config{})
	q := c.Client(t, 0)
	own := q.Conn().ConnectedUrl()

	if q.Conn().Opts.InProcessServer == nil {
		t.Fatalf("the node's connection to its own member dials %s over TCP, where "+
			"every connection to a member in this process is a pipe", own)
	}

	listener := c.Servers[0].ClientListener()
	if !strings.HasPrefix(listener, "127.0.0.1:") {
		t.Fatalf("member 0's client listener is %q, want loopback alone", listener)
	}
	// THE MESH IS WHOLE before the control reads anything ([StartCluster]
	// waits for every member to be routed to its peers), so the INFO the
	// control is handed on connecting already carries every peer a member
	// would advertise; the round trip after it lets any INFO sent since land.
	control, err := nats.Connect("nats://"+listener, operatorKey(t), nats.MaxReconnects(-1),
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

	// A BUDGET AT ALL: a connection that reconnects for ever is never closed,
	// so the node runs on dark after its member has stopped instead of
	// stopping for its supervisor to start the two again.
	if attempts := q.Conn().Opts.MaxReconnect; attempts < 0 {
		t.Fatalf("the node's connection to its own member reconnects for ever, so "+
			"with that member stopped it is never closed and the node runs on "+
			"with no broker (MaxReconnects %d)", attempts)
	}
	// THE CLIENT'S OWN WORD THAT IT IS DOWN, and any reconnect after it. The
	// handlers the dial installed still run; the case hears of each first.
	down := make(chan struct{})
	var downOnce sync.Once
	dialledDown := q.Conn().DisconnectErrHandler()
	q.Conn().SetDisconnectErrHandler(func(nc *nats.Conn, err error) {
		if dialledDown != nil {
			dialledDown(nc, err)
		}
		downOnce.Do(func() { close(down) })
	})
	landed := make(chan string, 1)
	dialled := q.Conn().ReconnectHandler()
	q.Conn().SetReconnectHandler(func(nc *nats.Conn) {
		select {
		case landed <- nc.ConnectedUrl():
		default:
		}
		if dialled != nil {
			dialled(nc)
		}
	})

	c.Servers[0].Shutdown()
	select {
	case <-down:
	case <-time.After(30 * time.Second): //fixedwait:liveness
		t.Fatalf("the node's connection did not say it was down within 30 s of its "+
			"own member %s stopping: it is %v", own, q.Conn().Status())
	}
	// ITS POOL WHERE NOTHING CAN ADD TO IT: what every reconnect from here is
	// to, so a peer it does not name is one no reconnect reaches.
	if pool := q.Conn().Servers(); len(pool) != 1 || pool[0] != own {
		t.Errorf("the node's connection would reconnect to %v, want its own "+
			"member %s alone", pool, own)
	}
	if found := q.Conn().DiscoveredServers(); len(found) != 0 {
		t.Errorf("the node's connection was told of %v, where a member "+
			"advertises no peer", found)
	}
	// AND DOWN, landed nowhere.
	if status := q.Conn().Status(); status == nats.CONNECTED {
		t.Fatalf("the node's connection is up on %s with its own member %s stopped",
			q.Conn().ConnectedUrl(), own)
	}
	select {
	case at := <-landed:
		t.Fatalf("the node's connection reconnected on %s with its own member %s "+
			"stopped", at, own)
	default:
	}
}
