package jetstreamtest

import (
	"context"
	"fmt"
	"testing"
)

// Relays is a partitionable route MESH with no servers in it — the addressing
// half of [StartPartitionableCluster], for callers that start their own
// members.
//
// # Why this exists apart from Cluster
//
// [StartPartitionableCluster] starts the members itself, which is right when
// what is under test is the broker. It is wrong when what is under test is a
// FLEET OF ENGINES: an engine embeds its own server and builds it from Tier A,
// so a harness that started the servers would be testing a topology no
// deployment has — engines talking to brokers they did not start.
//
// What a caller actually needs from that harness is the addressing: a shared
// cluster name, a route port per member, the peer URLs that reach the other
// members THROUGH a relay this process can cut, and an advertised address that
// is deliberately dead so gossip cannot route around the relays. That is this.
//
// The relays are running when this returns and are stopped when the test ends.
// [Relays.Partition] and [Relays.Heal] are [Cluster.Partition] and
// [Cluster.Heal] — the same forwarders, the same guarantee that a partition
// cuts established connections rather than only refusing new ones.
type Relays struct {
	c           *Cluster
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
func StartRelays(t *testing.T, n int) *Relays {
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
	withFreshPorts(t, "relay mesh", func(ctx context.Context) (*Cluster, error) {
		r, err := startRelaysOnce(ctx, t, n)
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
func startRelaysOnce(ctx context.Context, t *testing.T, n int) (*Relays, error) {
	t.Helper()
	ports := freePorts(ctx, t, n+n*(n-1)+n)
	routePorts, relayPorts, dead := ports[:n], ports[n:n+n*(n-1)], ports[n+n*(n-1):]

	c := &Cluster{}
	mesh := &Relays{c: c, ports: routePorts, dead: dead}
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
			// RETURNED, NOT FATAL. A relay listener loses its port to
			// the same race a member's does — this reserves n(n-1)+2n
			// of them, so it loses MORE often — and the retry above
			// tries again with a fresh set. t.Fatalf ends the test
			// binary's goroutine instead, so the retry that exists for
			// exactly this never runs.
			//
			// AND CLASSIFIED, through [listenErr]: a port somebody else
			// holds is worth another set of numbers, and a permission
			// failure, an address this host does not have or a
			// cancelled context answer identically however many it is
			// offered. This one call is what keeps the sentinel meaning
			// what its doc in [internal/queue/jetstream] says.
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
// # Why this is the default shape and the relay one is not
//
// A relay mesh costs n(n-1) listeners and copies every route byte through user
// space, and it forces each member to advertise a DEAD address so gossip
// cannot route around the relays — which is what makes a partition real. That
// is exactly right when a case cuts a member off, and it is pure cost, and
// pure risk, under a case that never does: raft traffic for every stream group
// in the company crossing a hand-written proxy adds a failure mode the
// deployment under test does not have.
//
// So a case that partitions takes [StartRelays] and pays for it. Everything
// else takes this.
func StartDirectMesh(t *testing.T, n int) *Relays {
	t.Helper()
	if n < 2 {
		t.Fatalf("StartDirectMesh(%d): use one member's own config for a solo node", n)
	}
	return &Relays{ports: freePorts(t.Context(), t, n), direct: true}
}

// Stop releases everything this mesh holds — the relay listeners, and with
// them the right to the ports it reserved.
//
// # Why a caller needs this rather than the test's own cleanup
//
// A harness that RETRIES a cluster start has to reserve fresh ports for each
// attempt, because the whole reason an attempt lost is that somebody else took
// a number this one was given, and asking for the same number again is asking
// for the same answer (see [ClusterStartAttempts], whose remedy is explicitly
// "trying again with different numbers"). A mesh per attempt means the failed
// attempt's relays have to go down when that attempt does — `t.Cleanup` runs
// when the TEST ends, which is after every attempt, so it is the wrong moment
// by exactly the interval that matters.
//
// Safe to call twice and safe on a direct mesh, which holds no listeners at
// all: the cleanup registered at construction calls it again.
func (r *Relays) Stop() {
	if r.c != nil {
		r.c.shutdown()
	}
}

// RelayClusterName is the cluster name every member shares.
const RelayClusterName = "crewlet-test"

// Member is member i's routing: the port it listens on, the peer URLs it
// dials, and the address it advertises to gossip.
//
// # Why the advertised address is dead
//
// NATS gossips the addresses members advertise, and a member that advertised
// its REAL route port would give its peers a path that does not run through a
// relay — so a partition would cut the configured routes and the gossiped one
// would carry on working, and every assertion under it would hold vacuously.
// An implicit route is attempted once and abandoned (nats-server retries only
// explicit ones), so a dead address costs one refused dial per gossip.
func (r *Relays) Member(i int) (port int, peers []string, advertise string) {
	if r.direct {
		// EVERY MEMBER'S REAL PORT, this one's included. NATS ignores a
		// route to itself, and listing them uniformly means a member's
		// peer list does not depend on its own index — which is what a
		// deployment writes, and what a `peers:` block in Tier A holds.
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
