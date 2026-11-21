package jetstreamtest

import (
	"context"
	"fmt"
	"testing"
)

// Relays is a partitionable route MESH with no servers in it: the addressing
// half of [StartPartitionableCluster], for callers that start their own
// members. A FLEET OF ENGINES needs that, since an engine embeds a server
// built from Tier A and harness-started servers would be a topology no
// deployment has. It gives a cluster name no other mesh carries, a route port
// per member, peer URLs reaching the others THROUGH a relay this process can
// cut, and a deliberately dead advertised address.
//
// The relays run when this returns and stop when the test ends.
// [Relays.Partition] and [Relays.Heal] are [Cluster.Partition] and
// [Cluster.Heal]: a partition cuts established connections too.
type Relays struct {
	c           *Cluster
	name        string
	ports, dead []int

	// direct is a mesh with no relays in it: every member is given its
	// peers' REAL route ports, so routes are ordinary NATS routes. See
	// [StartDirectMesh] for when that is the right trade.
	direct bool
}

// StartRelays reserves an n-member mesh and starts every relay in it.
//
// It reserves all three port sets TOGETHER, for the reason [freePorts] gives:
// taking them one at a time can hand out the same number twice, and the
// collision does not surface until a member silently forms no route.
// ctx BOUNDS THE MESH START, and a caller inside its own retry loop passes
// that loop's attempt context: see [withFreshPorts] for why a nested budget
// must share the outer one rather than stack beside it.
func StartRelays(ctx context.Context, t *testing.T, n int) *Relays {
	t.Helper()
	if n < 2 {
		t.Fatalf("StartRelays(%d): a partition needs at least two members, or "+
			"there is no pair to cut", n)
	}
	// THROUGH [withFreshPorts], the harness's ONE retry loop, rather than a
	// second one beside it. This reserves three port sets at once, so it is
	// the likelier half to lose one — but a second loop was a second set of
	// answers to "which failures are worth another attempt", and it got
	// them wrong in the direction the shared one is written against: it
	// retried everything and logged every failure as a lost port race, so
	// an unbindable address or a cancelled context cost four attempts and
	// was reported as a race the run had no evidence for.
	//
	// The classification it needs lives at the bind itself now, in
	// [listenErr], which is the only place that can tell those apart.
	var mesh *Relays
	withFreshPorts(ctx, t, "relay mesh", func(ctx context.Context, name string) (*Cluster, error) {
		r, err := startRelaysOnce(ctx, t, n, name)
		mesh = r
		return r.c, err
	})
	return mesh
}

// startRelaysOnce is one attempt, which is the unit the retry above works in.
//
// The PARTIAL MESH goes back with the error, never nil, for the reason
// [StartCluster]'s factory gives: the relays that DID bind hold their
// listeners until the test ends, and [withFreshPorts] is what takes them down
// between attempts. Only the embedded cluster is meaningful on an error.
func startRelaysOnce(ctx context.Context, t *testing.T, n int, name string) (*Relays, error) {
	t.Helper()
	ports := freePorts(ctx, t, n+n*(n-1)+n)
	routePorts, relayPorts, dead := ports[:n], ports[n:n+n*(n-1)], ports[n+n*(n-1):]

	c := &Cluster{}
	mesh := &Relays{c: c, name: name, ports: routePorts, dead: dead}
	for i := range n {
		for j := range n {
			if i == j {
				continue
			}
			c.forwarders = append(c.forwarders, &forwarder{
				from: i, to: j,
				target: hostPort(routePorts[j]),
				port:   relayPorts[len(c.forwarders)],
			})
		}
	}
	// STARTED BEFORE ANY MEMBER, so a member's first route dial has
	// somewhere to land. A relay's own dial to a peer that is not up yet
	// fails and the route is retried, which is the ordinary case NATS
	// already handles.
	for _, f := range c.forwarders {
		// ON WithoutCancel: a forwarder is a LISTENER that serves for
		// the whole case, while ctx ends when the bring-up does.
		// Bound to the attempt it was torn down the instant the attempt
		// succeeded, so every member came up into a mesh whose relays
		// were already cut — measured as
		// TestAPartitionedMemberIsSilentRatherThanSlow failing all four
		// attempts on "routed to [] (want 1 peers)". The values are
		// inherited; only the cancellation is not.
		if err := f.start(context.WithoutCancel(ctx)); err != nil {
			// RETURNED, NOT FATAL: a relay listener loses its port to
			// the same race a member's does (n(n-1)+2n of them, so more
			// often) and the retry above tries a fresh set; t.Fatalf
			// would skip that retry.
			//
			// CLASSIFIED through [listenErr]: a port somebody else holds
			// is worth another set of numbers; a permission failure, a
			// missing address or a cancelled context are not. This call
			// keeps the sentinel meaning what its doc in
			// [internal/queue/jetstream] says.
			return mesh, fmt.Errorf("start forwarder %d->%d: %w",
				f.from, f.to, listenErr(err))
		}
	}
	t.Cleanup(c.shutdown)
	return mesh, nil
}

// StartDirectMesh reserves an n-member mesh with NO relays: members are given
// each other's real route ports and talk directly.
//
// This is the default shape: a relay mesh costs n(n-1) listeners, copies every
// route byte through user space and forces a DEAD advertised address. That is
// right for a case that cuts a member off and pure cost and risk otherwise:
// raft traffic crossing a hand-written proxy is a failure mode the deployment
// under test does not have. A case that partitions takes [StartRelays].
// ctx bounds the port reservation, for [StartRelays]'s reason.
func StartDirectMesh(ctx context.Context, t *testing.T, n int) *Relays {
	t.Helper()
	if n < 2 {
		t.Fatalf("StartDirectMesh(%d): use one member's own config for a solo node", n)
	}
	return &Relays{name: newClusterName(), ports: freePorts(ctx, t, n), direct: true}
}

// Stop releases everything this mesh holds — the relay listeners, and with
// them the right to the ports it reserved.
//
// A harness that RETRIES a cluster start reserves fresh ports per attempt (see
// [ClusterStartAttempts]), so a failed attempt's relays must go down when that
// attempt does; `t.Cleanup` runs when the TEST ends, after every attempt.
//
// Safe to call twice and safe on a direct mesh, which holds no listeners at
// all: the cleanup registered at construction calls it again.
func (r *Relays) Stop() {
	if r.c != nil {
		r.c.shutdown()
	}
}

// ClusterName is the cluster name every member of this mesh carries — this
// mesh's alone, for [newClusterName]'s reason: a member a case stops for good
// leaves its peers dialling its route port, and a member another mesh is
// handed that port for must be refused by them rather than routed to.
func (r *Relays) ClusterName() string { return r.name }

// Member is member i's routing: the port it listens on, the peer URLs it
// dials, and the address it advertises to gossip.
//
// The advertised address is dead: a REAL one would give peers a path around
// the relays, so a partition would cut the configured routes while the
// gossiped one kept working and every assertion under it would hold
// vacuously. An implicit route is attempted once and abandoned (nats-server
// retries only explicit ones), so a dead address costs one refused dial.
func (r *Relays) Member(i int) (port int, peers []string, advertise string) {
	if r.direct {
		// EVERY MEMBER'S REAL PORT, this one's included. NATS ignores a
		// route to itself, and listing them uniformly means a member's
		// peer list does not depend on its own index.
		//
		// A HARNESS CONVENIENCE, NOT THE SHAPE TIER A ASKS FOR:
		// `stream.cluster.peers` names the OTHER members, because the
		// config counts them to decide whether a fleet has a quorum and
		// how many copies it can keep. A Tier A entry recognisably this
		// node's own route is discounted from that count with a warning,
		// and one that is not recognisable — an alias of this host — is
		// counted as a member it is not. This list reaches the embedded
		// broker directly and is counted by nothing.
		for _, p := range r.ports {
			peers = append(peers, routeURL(p))
		}
		return r.ports[i], peers, ""
	}
	for _, f := range r.c.forwarders {
		if f.from == i {
			peers = append(peers, routeURL(f.port))
		}
	}
	return r.ports[i], peers, hostPort(r.dead[i])
}

// Partition cuts member i off from every peer, in both directions — see
// [Cluster.Partition] for what that means and why it closes the connections
// rather than only the listeners.
//
// A mesh with no relays FAILS here rather than cutting nothing, which is the
// same guard [Cluster.requireForwarders] makes and for the same reason: a
// Partition that returns cleanly having cut nothing leaves every assertion
// under it holding vacuously.
func (r *Relays) Partition(t *testing.T, i int) {
	t.Helper()
	r.requireRelays(t)
	r.c.Partition(t, i)
}

// Heal restores member i's routes.
func (r *Relays) Heal(t *testing.T, i int) {
	t.Helper()
	r.requireRelays(t)
	r.c.Heal(t, i)
}

func (r *Relays) requireRelays(t *testing.T) {
	t.Helper()
	if r.direct || r.c == nil {
		t.Fatal("this mesh was built with StartDirectMesh, so Partition and " +
			"Heal would cut nothing and every assertion under them would hold " +
			"vacuously: build it with StartRelays")
	}
}

// Describe renders the mesh, for a failure message that has to say which
// member could reach which.
func (r *Relays) Describe() string {
	if r.direct {
		return fmt.Sprintf("direct mesh, route ports %v", r.ports)
	}
	out := ""
	for _, f := range r.c.forwarders {
		state := "up"
		f.mu.Lock()
		if f.closed || f.listener == nil {
			state = "cut"
		}
		f.mu.Unlock()
		out += fmt.Sprintf("%d->%d %s; ", f.from, f.to, state)
	}
	return out
}
