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
// A member advertises every peer's client URL, and a connection pooling them
// reconnects to a peer when it drops: a node whose member died ran on through a
// peer, a replica short. This holds that the node's connection is IN PROCESS,
// its pool is this member alone, and the loopback client listener tells NO
// client of any peer (the CONTROL, nats defaults, reads them all).
// ONCE ITS MEMBER STOPS it lands nowhere, decided by the pool, not a wait: the
// pool grows only by an INFO a connected reader takes.
// Mutations: NoAdvertise off → control discovers peers; dial over the client
// listener → in-process row; drop IgnoreDiscoveredServers and NoAdvertise →
// pool row; reconnect for ever → budget row.
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
