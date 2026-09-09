// Package jetstreamtest starts a real multi-node embedded NATS cluster, so a
// test can run against the topology a fleet actually deploys rather than
// against several clients of one server.
//
// The difference is not cosmetic. One server with three clients exercises no
// replication, no quorum, no leader election and no placement — and those are
// precisely the mechanisms a fleet depends on and a solo node never touches.
// A suite that only ever ran the single-server form certified, among other
// things, an embedded server hardcoded to one name, which NATS requires to be
// unique per cluster: every node in a real deployment would have refused its
// peers' routes.
package jetstreamtest

import (
	"context"
	"fmt"
	"net"
	"testing"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// Cluster is a running embedded NATS cluster.
type Cluster struct {
	// Servers are the members, in the order they were started.
	Servers []*js.Server
	// Configs are the per-member configs, so a caller can build a client
	// with the same cluster settings the member was started with.
	Configs []js.Config

	// forwarders are the per-ordered-pair relays every route runs through
	// on a cluster started by StartPartitionableCluster, and empty on one
	// started by StartCluster. See partition.go.
	forwarders []*forwarder
}

// StartCluster starts an n-member embedded cluster and waits for it to form.
//
// Each member gets its own store directory and its own stable server name,
// exactly as n separate processes would. Streams default to n replicas, so a
// publish is quorum-durable before Publish returns — which is the property
// that makes "sync truth, async cache" true rather than aspirational.
//
// The cluster is shut down when the test ends.
func StartCluster(t *testing.T, n int, base js.Config) *Cluster {
	t.Helper()
	if n < 1 {
		t.Fatalf("StartCluster(%d): a cluster needs at least one member", n)
	}

	// Ports are reserved up front because every member's routes must name
	// every other member, including ones not started yet. Reserving by
	// binding and releasing is racy in principle; in practice the window
	// is microseconds and the alternative — starting members one at a time
	// and rewriting routes — is a NATS reload per member.
	ports := freePorts(t, n)
	routes := make([]string, n)
	for i, p := range ports {
		routes[i] = routeURL(p)
	}

	c := &Cluster{}
	for i := range n {
		cfg := memberConfig(base, i, n, ports[i], routes)
		c.start(t, cfg, i)
	}

	// No wait here: StartServer does not return a clustered member until
	// its JetStream is current, because a node that provisions into a
	// leaderless metadata group blocks rather than failing, and that is a
	// production boot hazard rather than a test one.
	return c
}

// StartPartitionableCluster starts an n-member cluster whose every route runs
// through a relay this process owns, so a test can cut one member off from
// the rest and put it back — see [Cluster.Partition].
//
// # Why this is not the default
//
// It costs n(n-1) extra listeners and copies every route byte through user
// space, so it is for the cases that need a partition and no others. Those
// are the cases about a member that is UNREACHABLE rather than stopped, and
// the two are not the same input: a stopped member's peers watch its routes
// close and its leases lapse, while a partitioned member carries on believing
// it holds everything it held and comes back still believing it.
//
// # Why gossip is aimed at a dead port
//
// A NATS member tells its peers where to find the members it has just met,
// and each of them dials that address ITSELF. With nothing configured, the
// address is derived from the connection's own remote address — which here is
// the relay's, carrying the peer's REAL route port, so a third member would
// dial straight past the relay and hold a route no partition could cut. That
// is the same vacuous pass [Cluster.Partition] guards against, arriving by a
// different door. So every member advertises a port nothing listens on: a
// gossiped route can never be established, the configured relays are the only
// path between members, and cutting them cuts everything. An implicit route
// is attempted once and abandoned (nats-server retries only explicit ones),
// so the dead address costs one refused dial per gossip and no reconnect
// storm.
func StartPartitionableCluster(t *testing.T, n int, base js.Config) *Cluster {
	t.Helper()
	if n < 2 {
		t.Fatalf("StartPartitionableCluster(%d): a partition needs at least "+
			"two members, or there is no pair to cut", n)
	}

	// Three port sets, reserved together for the reason freePorts exists:
	// the members' real route ports, one relay port per ordered pair, and
	// one dead port per member for the address it advertises.
	ports := freePorts(t, n+n*(n-1)+n)
	routePorts, relays, dead := ports[:n], ports[n:n+n*(n-1)], ports[n+n*(n-1):]

	c := &Cluster{}
	for i := range n {
		for j := range n {
			if i == j {
				continue
			}
			c.forwarders = append(c.forwarders, &forwarder{
				from: i, to: j,
				target: hostPort(routePorts[j]),
				port:   relays[len(c.forwarders)],
			})
		}
	}
	// Started before the members, so a member's first route dial has
	// somewhere to land. The relay's own dial to a peer that is not up yet
	// fails and the route is retried, which is the ordinary case NATS
	// already handles.
	for _, f := range c.forwarders {
		if err := f.start(); err != nil {
			t.Fatalf("start forwarder %d->%d: %v", f.from, f.to, err)
		}
	}
	t.Cleanup(func() {
		for _, f := range c.forwarders {
			f.stop()
		}
	})

	for i := range n {
		var routes []string
		for _, f := range c.forwarders {
			if f.from == i {
				routes = append(routes, routeURL(f.port))
			}
		}
		cfg := memberConfig(base, i, n, routePorts[i], routes)
		// Loopback rather than every interface: the relays dial
		// 127.0.0.1, and a member listening wider would be reachable
		// from outside the harness on a port a partition does not cut.
		cfg.ClusterHost = "127.0.0.1"
		cfg.ClusterAdvertise = hostPort(dead[i])
		c.start(t, cfg, i)
	}
	return c
}

// Client connects a queue to member i. Each engine node in a fleet talks to
// the member embedded in its own process, which is what this models.
func (c *Cluster) Client(t *testing.T, i int) *js.Queue {
	t.Helper()
	q, err := c.Servers[i].Client(t.Context())
	if err != nil {
		t.Fatalf("cluster member %d: client: %v", i, err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	return q
}

// memberConfig is member i's config: the caller's base, plus the identity and
// membership every member of an n-node cluster needs.
func memberConfig(base js.Config, i, n, clusterPort int, routes []string) js.Config {
	cfg := base
	cfg.ServerName = fmt.Sprintf("crewlet-test-%d", i)
	cfg.ClusterName = "crewlet-test"
	cfg.ClusterPort = clusterPort
	cfg.ClusterURLs = routes
	if cfg.Replicas == 0 {
		cfg.Replicas = n
	}
	return cfg
}

// start brings up member i and registers its shutdown.
func (c *Cluster) start(t *testing.T, cfg js.Config, i int) {
	t.Helper()
	cfg.StoreDir = t.TempDir()
	srv, err := js.StartServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("cluster member %d: %v", i, err)
	}
	t.Cleanup(srv.Shutdown)
	c.Servers = append(c.Servers, srv)
	c.Configs = append(c.Configs, cfg)
}

// routeURL is a member's route address as NATS spells it.
func routeURL(port int) string { return "nats://" + hostPort(port) }

// hostPort is a loopback address for a port on this host.
func hostPort(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// freePorts reserves n ports the OS is not using.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	// Held open together, then all released: taking and releasing one at a
	// time can hand out the same port twice.
	listeners := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	var lc net.ListenConfig
	for range n {
		l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve a port: %v", err)
		}
		listeners = append(listeners, l)
		//nolint:errcheck // Listen on a TCP address always yields *TCPAddr.
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range listeners {
		_ = l.Close()
	}
	return ports
}
