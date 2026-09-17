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
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

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

	return withFreshPorts(t, "cluster", func(ctx context.Context) (*Cluster, error) {
		// Ports are reserved up front because every member's routes
		// must name every other member, including ones not started
		// yet, and the alternative — starting members one at a time
		// and rewriting routes — is a NATS reload per member.
		ports := freePorts(ctx, t, n)
		routes := make([]string, n)
		for i, p := range ports {
			routes[i] = routeURL(p)
		}

		c := &Cluster{}
		for i := range n {
			if err := c.start(ctx, t, memberConfig(base, i, n, ports[i], routes), i); err != nil {
				// THE PARTIAL CLUSTER GOES BACK WITH THE ERROR, so
				// [withFreshPorts] can take it down before retrying.
				// Discarded, the members that DID start keep their
				// route listeners — their shutdown is registered with
				// t.Cleanup and so runs at the end of the test, not at
				// the end of this attempt — and the next attempt then
				// draws ports from a machine still holding the old
				// ones. That is the contention the retry exists to
				// escape, left in place by the retry itself.
				return c, err
			}
		}

		// No wait here: StartServer does not return a clustered member
		// until its JetStream is current, because a node that
		// provisions into a leaderless metadata group blocks rather
		// than failing, and that is a production boot hazard rather
		// than a test one.
		return c, nil
	})
}

// ClusterStartAttempts is how many times a cluster is stood up before the
// harness gives up.
//
// EXPORTED BECAUSE THERE ARE TWO LAYERS AND ONE DECISION. [internal/e2e]
// retries a whole fleet of engines on the same reasoning — its own doc says so
// in as many words, "applied one layer up" — and carried its own number while
// saying it. Four here and three there is the drift this repository has
// already paid for under three other names (textcut, whsec, jsprovision): two
// spellings of one rule, each comment asserting it matched the other, with
// nothing enforcing it. One constant, read by both.
//
// # Why a retry rather than a wider window
//
// Reserving a port means binding it, reading the number and letting it go, so
// there is always an interval in which somebody else can take it — and the
// somebody else is usually the rest of this repository's own suite, which
// starts dozens of embedded brokers at once. Measured: this harness passes in
// six seconds on a loaded machine in isolation and timed out at a hundred and
// twenty inside a full run.
//
// A wider reservation window would not help, because the window is not the
// problem: the collision is with a process this one does not coordinate with.
// What DOES fix it is noticing — see [js.Server.ClusterPort] — and trying
// again with different numbers.
const ClusterStartAttempts = 4

// ClusterStartBudget bounds the WHOLE retry loop in wall clock, not just the
// number of tries.
//
// # Why attempts alone were not a bound
//
// Because n attempts at an unbounded cost each is a PRODUCT nobody declared,
// which is the lesson [jsprovision.SequenceBudget] already records one layer
// down: "the real worst case was already the product rather than the term".
// Measured on the engine's own CI: three cluster cases spent 438.93s, 324.11s
// and 314.48s — 1077s, 64% of the package's whole run — on three attempts
// each that were individually bounded and collectively not. The package
// timeout came within 115 seconds of firing, which would have replaced three
// legible test failures with one suite timeout naming nothing.
//
// # THREE MINUTES
//
// Above any legitimate bring-up by a wide margin: a healthy two-member fleet
// in [internal/e2e] boots, asserts and tears down in about ten seconds on a
// four-vCPU host, and the slowest cluster case there measures 63s END TO END
// — of which the bring-up this bounds is a fraction.
//
// And below the point where retrying costs more than it can buy: six cluster
// cases at this ceiling is eighteen minutes inside a thirty-minute package
// timeout, so even a run that loses every one of them reports what failed
// instead of being killed mid-test.
//
// It bounds the LOOP, and it does so by bounding each attempt within it: every
// attempt is handed a context carrying what is LEFT of the budget, so a member
// start that would otherwise wait out its own clustered accept budget is
// interrupted instead. Consulted only between attempts — which is how this was
// first written — it bounded how MANY were made and nothing about how long one
// took, and a single sequential three-member attempt could outlast the whole
// ceiling before the loop ever looked at it.
//
// What an attempt should cost when nothing is wrong is still the engine's
// question and [jsprovision] answers it; this is the harness's ceiling on
// re-asking it.
const ClusterStartBudget = 3 * time.Minute

// errNotRetryable marks a start failure that a fresh set of ports cannot
// change: the same input answering the same way every time.
//
// DELIBERATELY SHORT, and everything not on it is retried — see
// [withFreshPorts] for why the burden sits on proving a failure repeats.
var errNotRetryable = errors.New("not fixable by another attempt")

// listenErr classifies a route listener's bind failure for [withFreshPorts].
//
// THE ONE CLASSIFIER BOTH RELAY PATHS USE, so the retry gets the same answer
// wherever a forwarder failed to bind. [net.ListenConfig.Listen] hands back
// EADDRINUSE for the race this harness genuinely loses — it reserves
// n(n-1)+2n ports and then binds them one at a time — and a permission
// failure, an address this host does not have, or a cancelled context for the
// things a different number answers identically.
//
// It exists because the classification was written at neither relay path:
// both returned the raw error, so [withFreshPorts]'s "retry unless it is
// proven to repeat" read every one of them as transient and spent four
// attempts on an unbindable address before reporting it as a lost race. The
// mechanism was already here — [Cluster.start] classifies its own probe
// failure exactly this way — and the relay half simply did not feed it.
func listenErr(err error) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("%w: %w", js.ErrRoutePortTaken, err)
	}
	return fmt.Errorf("%w: %w", errNotRetryable, err)
}

// withFreshPorts runs start until it comes up, with fresh ports each time.
//
// # What it retries, and what it must not
//
// RETRY IS THE DEFAULT and the log says what actually failed. Standing up n
// brokers fails for TIMING far more often than for ports — a member whose
// readiness wait ran out, a metadata group still electing — and absorbing
// exactly that is what the attempts are for. Measured by getting it backwards
// in this same pull request: gating [internal/e2e]'s equivalent retry on a
// lost port alone ended three cluster cases on their first attempt, each on a
// `context deadline exceeded` that a second attempt had always taken in its
// stride.
//
// So only a failure KNOWN to repeat ends the run early, and the burden is on
// proving that rather than on proving it might not: guessing "deterministic"
// wrongly ends a case that would have passed, while guessing "transient"
// wrongly costs some seconds and a line.
//
// What the log must not do is call every one of them a port race. That was the
// other half of the same defect — the one diagnostic a reader gets, naming a
// cause the run had no evidence for.
func withFreshPorts(t *testing.T, what string,
	start func(context.Context) (*Cluster, error)) *Cluster {

	t.Helper()
	var last error
	// THE DEADLINE IS HANDED TO THE ATTEMPT, not merely consulted after
	// it. Checked only between attempts it bounded how many were made and
	// nothing about how long one took: a member start reaching
	// [js.StartServer] on an unbounded context waits its own clustered
	// accept budget, and the members are started in sequence, so a single
	// attempt could outlast this whole ceiling before the loop below ever
	// looked at it. A budget the thing it bounds cannot observe is a
	// comment, not a bound.
	deadline := time.Now().Add(ClusterStartBudget)
	for attempt := 1; attempt <= ClusterStartAttempts; attempt++ {
		attemptCtx, cancelAttempt := context.WithDeadline(t.Context(), deadline)
		c, err := start(attemptCtx)
		cancelAttempt()
		if err == nil {
			return c
		}
		last = err
		// EVERYTHING THIS ATTEMPT STARTED GOES DOWN NOW, not at the end
		// of the test: a half-started cluster still holds the ports the
		// next attempt is about to ask for.
		if c != nil {
			c.shutdown()
		}
		if errors.Is(err, errNotRetryable) {
			t.Fatalf("%s attempt %d/%d failed for a reason retrying "+
				"cannot fix: %v", what, attempt, ClusterStartAttempts, err)
		}
		// THE CEILING IS CHECKED AFTER THE ATTEMPT, so a budget that
		// expires never costs a try that was already paid for — and
		// before the log line, so the last thing a reader sees names
		// the bound rather than a retry that never happened.
		if time.Now().After(deadline) {
			t.Fatalf("no %s came up within %s (%d of %d attempts): %v — the "+
				"retry is bounded in wall clock as well as in tries, because "+
				"n attempts at an unbounded cost each is a product nobody "+
				"declared", what, ClusterStartBudget, attempt,
				ClusterStartAttempts, last)
		}
		t.Logf("%s attempt %d/%d failed, retrying with fresh ports: %v",
			what, attempt, ClusterStartAttempts, err)
	}
	t.Fatalf("no %s came up in %d attempts: %v — which is what a "+
		"machine running many brokers at once produces",
		what, ClusterStartAttempts, last)
	return nil
}

// shutdown stops everything this cluster started, and is safe to call twice:
// the test's own cleanup calls it again.
func (c *Cluster) shutdown() {
	for _, f := range c.forwarders {
		f.stop()
	}
	for _, s := range c.Servers {
		s.Shutdown()
	}
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

	return withFreshPorts(t, "partitionable cluster", func(ctx context.Context) (*Cluster, error) {
		return startPartitionable(ctx, t, n, base)
	})
}

func startPartitionable(ctx context.Context, t *testing.T, n int, base js.Config) (*Cluster, error) {
	t.Helper()
	// Three port sets, reserved together for the reason freePorts exists:
	// the members' real route ports, one relay port per ordered pair, and
	// one dead port per member for the address it advertises.
	ports := freePorts(ctx, t, n+n*(n-1)+n)
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
		if err := f.start(ctx); err != nil {
			// THROUGH [listenErr], because the retry above cannot tell a
			// lost port from an unbindable address on its own — and the
			// raw error told it everything was transient.
			return c, fmt.Errorf("start forwarder %d->%d: %w",
				f.from, f.to, listenErr(err))
		}
	}
	t.Cleanup(c.shutdown)

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
		if err := c.start(ctx, t, cfg, i); err != nil {
			return c, err
		}
	}
	return c, nil
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
	// LOOPBACK, because that is what the routes name: hostPort builds
	// 127.0.0.1, so a member listening on every interface is reachable
	// from outside the harness on a port nothing here controls — and the
	// pre-bind probe, which asks about one address, cannot answer for a
	// wildcard bind at all. The relay path has always set this; the direct
	// one did not, so its probe could pass on 127.0.0.1 while the bind
	// lost :port to something holding another local interface.
	cfg.ClusterHost = "127.0.0.1"
	if cfg.Replicas == 0 {
		cfg.Replicas = n
	}
	return cfg
}

// start brings up member i and registers its shutdown.
//
// IT CHECKS THAT THE ROUTE LISTENER ACTUALLY BOUND, which nothing else does. A
// member whose route port is already taken starts, serves clients and passes
// every readiness check — it just never forms a route, which from the outside
// is indistinguishable from a cluster that is slow to converge. That is how it
// was found: a harness waiting out its whole budget for peers that could never
// arrive, reporting only "routed to [], want 2".
// ctx BOUNDS THIS MEMBER'S START, and is the attempt's rather than the test's:
// see [withFreshPorts] for why a ceiling the start cannot observe bounds
// nothing.
func (c *Cluster) start(ctx context.Context, t *testing.T, cfg js.Config, i int) error {
	t.Helper()
	cfg.StoreDir = t.TempDir()
	// PROBED IMMEDIATELY BEFORE THE SERVER BINDS IT, because the interval
	// between reserving a port and using it is what got long: this harness
	// reserves twelve at once and then starts n(n-1) relays before the
	// first member asks for one. The probe cannot close the race — nothing
	// can, short of never letting the port go — but it shortens the window
	// from seconds to microseconds, and it turns the loss from a
	// two-minute readiness timeout into an immediate retry.
	switch free, err := PortFree(ctx, cfg.ClusterHost, cfg.ClusterPort); {
	case err != nil:
		// NOT A RACE, so the retry above must not treat it as one: an
		// address this host does not have, or a probe that never ran.
		// Both answer identically however many ports it is offered.
		return fmt.Errorf("cluster member %d: %w: route port %d on %q cannot "+
			"be probed: %w", i, errNotRetryable, cfg.ClusterPort,
			cfg.ClusterHost, err)
	case !free:
		return fmt.Errorf("cluster member %d: %w — route port %d went between "+
			"this harness reserving it and the member starting",
			i, js.ErrRoutePortTaken, cfg.ClusterPort)
	}
	srv, err := js.StartServer(ctx, cfg)
	if err != nil {
		return fmt.Errorf("cluster member %d: %w", i, err)
	}
	t.Cleanup(srv.Shutdown)
	c.Servers = append(c.Servers, srv)
	c.Configs = append(c.Configs, cfg)
	if got := srv.ClusterPort(); got != cfg.ClusterPort {
		return fmt.Errorf("cluster member %d was given route port %d and bound "+
			"%d — its listener lost the port between this harness reserving it "+
			"and the server binding it, so it can never form a route",
			i, cfg.ClusterPort, got)
	}
	return nil
}

// routeURL is a member's route address as NATS spells it.
func routeURL(port int) string { return "nats://" + hostPort(port) }

// hostPort is a loopback address for a port on this host.
func hostPort(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// PortFree reports whether a port can still be bound right now.
//
// It is a probe rather than a reservation: what it buys is finding a lost race
// in microseconds instead of two minutes. A clustered member whose route
// listener cannot bind does not fail fast — it starts, serves clients, never
// forms a route, and is only reported when its readiness budget expires.
//
// EXPORTED because the members are not always this package's to start. A fleet
// of ENGINES embeds its own servers and builds them from Tier A (see [Relays]),
// so the harness that stands one up cannot call [Cluster.start] and would
// otherwise have no way to make the same check — which is exactly the gap it
// had.
// THE HOST IS AN ARGUMENT, because a probe that assumes one is a probe that
// can answer about an address the server never binds — which is exactly what a
// hardcoded 127.0.0.1 did for a member left to listen on every interface.
//
// AND A PROBE THAT COULD NOT ANSWER IS NOT AN ANSWER, which is why the error
// comes back rather than being folded into the bool. [js.PortAvailable] draws
// that line deliberately — an occupied port is (false, nil) and an address
// this host does not have, a privileged port or a cancelled probe is
// (false, err) — and collapsing it here put every one of those back under "the
// port was taken", so a caller retried a configuration mistake three times and
// then reported a race. That is the exact sentence [js.PortAvailable]'s own
// doc says it exists to prevent.
func PortFree(ctx context.Context, host string, port int) (bool, error) {
	// THE ENGINE'S OWN PROBE, not a second one: [js.PortAvailable] is what
	// a clustered member runs against its configured route port before it
	// starts, and a harness asking the same question a different way is how
	// one answer stops matching the other.
	return js.PortAvailable(ctx, host, port)
}

// freePorts reserves n ports the OS is not using.
// ctx bounds the reservation, so an attempt whose wall-clock ceiling has
// expired does not start by asking the kernel for ports it will not use.
func freePorts(ctx context.Context, t *testing.T, n int) []int {
	t.Helper()
	// Held open together, then all released: taking and releasing one at a
	// time can hand out the same port twice.
	listeners := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	var lc net.ListenConfig
	for range n {
		l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
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
