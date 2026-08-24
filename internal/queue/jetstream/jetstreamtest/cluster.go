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
		routes[i] = fmt.Sprintf("nats://127.0.0.1:%d", p)
	}

	c := &Cluster{}
	for i := range n {
		cfg := base
		cfg.ServerName = fmt.Sprintf("crewlet-test-%d", i)
		cfg.ClusterName = "crewlet-test"
		cfg.ClusterPort = ports[i]
		cfg.ClusterURLs = routes
		cfg.StoreDir = t.TempDir()
		if cfg.Replicas == 0 {
			cfg.Replicas = n
		}

		srv, err := js.StartServer(cfg)
		if err != nil {
			t.Fatalf("StartCluster: member %d: %v", i, err)
		}
		t.Cleanup(srv.Shutdown)
		c.Servers = append(c.Servers, srv)
		c.Configs = append(c.Configs, cfg)
	}

	// No wait here: StartServer does not return a clustered member until
	// its JetStream is current, because a node that provisions into a
	// leaderless metadata group blocks rather than failing, and that is a
	// production boot hazard rather than a test one.
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

// freePorts reserves n ports the OS is not using.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	// Held open together, then all released: taking and releasing one at a
	// time can hand out the same port twice.
	listeners := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
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
