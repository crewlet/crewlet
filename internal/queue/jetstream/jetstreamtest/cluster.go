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
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/runtoken"
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

	// life is the test the cluster was started by, which a member brought
	// back by [Cluster.RestartMember] stops with — whatever test restarted
	// it. A cluster outlives the cases run on it, and a member, or a client
	// of it, stopped at the end of the case that restarted it would leave the
	// next case a cluster with a member missing.
	life testing.TB
}

// clusterSeq numbers the clusters this process names, so no two share one.
var clusterSeq atomic.Uint64

// newClusterName is a cluster name no other cluster, in this process or
// another, carries. Every bring-up ATTEMPT is handed one ([withFreshPorts],
// [Relays.ClusterName]).
//
// A member shut down for good leaves its peers redialling its EXPLICIT route
// port for ever (server/route.go, tryForEver), and [freePorts] can hand that
// port to a cluster starting in the same binary. Under one shared name the
// member joins that mesh, and server names and so JetStream peer ids
// ([js.PeerIDOf] hashes the name) collide. A route with another cluster name
// is refused (processRouteConnect), so a name per cluster turns that merge
// into a refused dial. Process id and counter, never a port: a port is what a
// stopped member gives back.
func newClusterName() string {
	return fmt.Sprintf("crewlet-test-%d-%d", os.Getpid(), clusterSeq.Add(1))
}

// StartCluster starts an n-member embedded cluster and waits for it to form.
//
// Each member gets its own store directory and stable server name, as n
// separate processes would, and a store sized at [brokertest.StoreMaxBytes]
// unless base declares a size. Streams default to n replicas, so a publish is
// quorum-durable before Publish returns ("sync truth, async cache").
//
// It takes a [testing.TB] because a BENCHMARK stands up the same topology: a
// fleet broker's cost measured on another topology is worth nothing.
//
// The cluster is shut down when the test ends.
func StartCluster(t testing.TB, n int, base js.Config) *Cluster {
	t.Helper()
	return startCluster(t, n, base, false)
}

// startCluster is [StartCluster], optionally with a LEAF LISTENER on every
// member — which is where a node without the `data` role joins the fleet.
//
// A listener per member rather than one on member 0, because that is the
// topology [js.Config.LeafURLs] is written for: a leaf names every member and
// NATS picks one, so a fleet whose leaves all landed on one member would be
// measuring that member rather than the fleet. The ports are reserved WITH the
// route ports, for [freePorts]'s reason — reserved separately, the same number
// can be handed out twice.
func startCluster(t testing.TB, n int, base js.Config, leafListeners bool) *Cluster {
	t.Helper()
	if n < 2 {
		// ONE MEMBER IS NOT A CLUSTER THAT COMES UP: a member whose only
		// route is itself never elects a metadata leader — measured, its
		// JetStream answers "temporarily unavailable" from ten seconds on
		// and [js.Server.Client] never returns on it — so the wait below
		// could only spend the whole retry budget and report a port race.
		// A case that needs one broker starts it with [js.StartServer].
		t.Fatalf("StartCluster(%d): a cluster needs at least two members — one "+
			"never elects a metadata leader, so nothing could be provisioned on it", n)
	}

	return withFreshPorts(t.Context(), t, "cluster", func(ctx context.Context, name string) (*Cluster, error) {
		// Ports are reserved up front because every member's routes
		// must name every other member, including ones not started
		// yet, and the alternative — starting members one at a time
		// and rewriting routes — is a NATS reload per member.
		reserve := n
		if leafListeners {
			reserve = 2 * n
		}
		ports := freePorts(ctx, t, reserve)
		routes := make([]string, n)
		for i, p := range ports[:n] {
			routes[i] = routeURL(p)
		}

		c := &Cluster{life: t}
		for i := range n {
			cfg := memberConfig(base, name, i, n, ports[i], routes)
			if leafListeners {
				// LOOPBACK for memberConfig's reason: a listener
				// wider than the addresses the leaves are given is
				// reachable on a port nothing here controls.
				cfg.LeafHost = "127.0.0.1"
				cfg.LeafPort = ports[n+i]
			}
			if err := c.start(ctx, t, cfg, i); err != nil {
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

		return c, c.awaitReady(ctx)
	})
}

// awaitReady waits until every member can serve a stream at its configured
// replica count (current, routed, answered by the metadata leader).
//
// It runs AFTER EVERY MEMBER HAS STARTED because [js.StartServer] cannot wait:
// the first member of a fresh cluster would wait for a quorum its own blocking
// keeps from forming. Only [js.Server.Client] waited, so a test on a member's
// raw connection ([js.Server.Conn]) could send its first lookup to a group with
// no leader yet; that request is DROPPED, not refused, and sat out the
// fifteen-second ask term. A member that never gets there fails the ATTEMPT,
// so [withFreshPorts] tries again.
func (c *Cluster) awaitReady(ctx context.Context) error {
	for i, srv := range c.Servers {
		if err := srv.AwaitClusterReady(ctx, c.Configs[i].Replicas); err != nil {
			return fmt.Errorf("cluster member %d: %w", i, err)
		}
	}
	return nil
}

// LeafURLs is every member's leaf listener, in the form [js.Config.LeafURLs]
// takes — empty on a cluster started without them.
func (c *Cluster) LeafURLs() []string {
	var urls []string
	for _, cfg := range c.Configs {
		if cfg.LeafPort != 0 {
			urls = append(urls, fmt.Sprintf("nats-leaf://%s", hostPort(cfg.LeafPort)))
		}
	}
	return urls
}

// ClusterStartAttempts is how many times a cluster is stood up before the
// harness gives up.
//
// Exported because [internal/e2e] retries a whole fleet of engines on the same
// reasoning one layer up: two copies of one number drift, so both read this.
//
// A retry rather than a wider window: reserving a port means binding, reading
// the number and letting it go, so another process (usually the rest of this
// suite) can take it in between, and no window helps against a process this
// one does not coordinate with. What fixes it is noticing (the pre-start
// probe, the server's [js.ErrRoutePortTaken]) and trying again with new numbers.
const ClusterStartAttempts = 4

// ClusterStartBudget bounds the WHOLE retry loop in wall clock, not just the
// number of tries: n attempts at an unbounded cost each is a PRODUCT nobody
// declared ([jsprovision.SequenceBudget]).
//
// THREE MINUTES is far above any legitimate bring-up (a healthy two-member
// fleet in [internal/e2e] takes about ten seconds, the slowest cluster case
// 63s end to end) and below where retrying costs more than it buys: six cases
// at this ceiling are eighteen minutes inside a thirty-minute package timeout.
//
// Each attempt gets a context carrying what is LEFT, so a member start waiting
// out its unbounded store recovery is interrupted; checked only between
// attempts, one could outlast the ceiling.
const ClusterStartBudget = 3 * time.Minute

// ClusterStartTerm is what ONE attempt may spend. [ClusterStartBudget] alone
// hands the first attempt the whole remaining budget, so one that hangs (a
// broker retrying a route to a port where nothing listens) consumes every
// retry, and the remedy of new numbers is unreachable.
//
// It is the budget divided by the attempts, so nothing new is chosen: all four
// tries are real. At 45s it is still far above any healthy bring-up; a slow
// legitimate one loses an attempt and gets a fresh one with new ports.
const ClusterStartTerm = ClusterStartBudget / ClusterStartAttempts

// StartAttemptEnd is when one bring-up attempt must give up: the sooner of
// what is left of the whole budget and this attempt's own term.
//
// PURE OVER VALUES, for the reason `internal/textindex` and
// `internal/solo/partition` keep their arithmetic out of the I/O: a rule that
// can only be exercised by standing up three brokers is a rule nobody
// re-measures, and both retry loops — this package's and [internal/e2e]'s —
// read this one so the two cannot drift.
func StartAttemptEnd(now, deadline time.Time) time.Time {
	if deadline.Sub(now) > ClusterStartTerm { //fixedwait:measure the remaining time only sizes the bound
		return now.Add(ClusterStartTerm)
	}
	return deadline
}

// errNotRetryable marks a start failure that a fresh set of ports cannot
// change: the same input answering the same way every time.
//
// DELIBERATELY SHORT, and everything not on it is retried — see
// [withFreshPorts] for why the burden sits on proving a failure repeats.
var errNotRetryable = errors.New("not fixable by another attempt")

// listenErr classifies a route listener's bind failure for [withFreshPorts].
//
// It is the one classifier both relay paths use, so the retry gets the same
// answer wherever a forwarder failed to bind. [net.ListenConfig.Listen] hands
// back EADDRINUSE for the race this harness genuinely loses (it reserves
// n(n-1)+2n ports and binds them one at a time); a permission failure, an
// address this host lacks or a cancelled context fail identically under any
// number. A raw error would read as transient to [withFreshPorts]'s "retry
// unless proven to repeat" and spend every attempt on an unbindable address.
func listenErr(err error) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("%w: %w", js.ErrRoutePortTaken, err)
	}
	return fmt.Errorf("%w: %w", errNotRetryable, err)
}

// withFreshPorts runs start until it comes up, with fresh ports and a fresh
// cluster NAME ([newClusterName]) each attempt, minted here because every
// bring-up passes through.
//
// RETRY IS THE DEFAULT: bring-up fails for TIMING far more often than for ports
// (a readiness wait that ran out, a group still electing). Only a failure KNOWN
// to repeat ends the run early, since guessing "deterministic" wrongly ends a
// case that would pass. The log names what failed, never "a port race".
//
// ctx IS THE PARENT EVERY ATTEMPT DERIVES FROM, so a caller's budget is kept:
// WithDeadline only shortens, and a mesh start nested in another retry loop
// must SHARE its ceiling, not hand the outer loop an expired context.
func withFreshPorts(ctx context.Context, t testing.TB, what string,
	start func(ctx context.Context, name string) (*Cluster, error)) *Cluster {

	t.Helper()
	var last error
	// THE DEADLINE IS HANDED TO THE ATTEMPT, not merely consulted after
	// it. Checked only between attempts it bounded how many were made and
	// nothing about how long one took: a member start reaching
	// [js.StartServer] on an unbounded context waits out its whole store
	// recovery and then its listener budget, and the members are started
	// in sequence, so a single attempt could outlast this whole ceiling
	// before the loop below ever looked at it. A budget the thing it
	// bounds cannot observe is a comment, not a bound.
	deadline := time.Now().Add(ClusterStartBudget)
	for attempt := 1; attempt <= ClusterStartAttempts; attempt++ {
		// THE SOONER OF THE CEILING AND THIS ATTEMPT'S OWN TERM. Handed the
		// whole remaining budget — which is how this was written — attempt one
		// could spend all of it and the other three never ran. See
		// [ClusterStartTerm].
		attemptCtx, cancelAttempt := context.WithDeadline(ctx,
			StartAttemptEnd(time.Now(), deadline))
		c, err := start(attemptCtx, newClusterName())
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
		if time.Until(deadline) <= 0 { //fixedwait:measure the retry's own deadline, which only ends a bring-up that never came up
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
// the rest and put it back — see [Cluster.Partition]. It costs n(n-1) extra
// listeners, so it is for a member UNREACHABLE rather than stopped.
//
// Gossip is aimed at a dead port: a member tells its peers where to find the
// members it has met, and each dials that address ITSELF. By default it derives
// from the relay's address, carrying the peer's REAL route port, so a third
// member would dial past the relay and hold a route no partition cuts. Every
// member advertises a port nothing listens on instead; an implicit route is
// tried once and abandoned (nats-server retries only explicit ones).
func StartPartitionableCluster(t *testing.T, n int, base js.Config) *Cluster {
	t.Helper()
	if n < 2 {
		t.Fatalf("StartPartitionableCluster(%d): a partition needs at least "+
			"two members, or there is no pair to cut", n)
	}

	return withFreshPorts(t.Context(), t, "partitionable cluster", func(ctx context.Context, name string) (*Cluster, error) {
		return startPartitionable(ctx, t, n, base, name)
	})
}

func startPartitionable(ctx context.Context, t *testing.T, n int, base js.Config, name string) (*Cluster, error) {
	t.Helper()
	// Three port sets, reserved together for the reason freePorts exists:
	// the members' real route ports, one relay port per ordered pair, and
	// one dead port per member for the address it advertises.
	ports := freePorts(ctx, t, n+n*(n-1)+n)
	routePorts, relays, dead := ports[:n], ports[n:n+n*(n-1)], ports[n+n*(n-1):]

	c := &Cluster{life: t}
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
		// ON WithoutCancel: a forwarder is a LISTENER that serves for
		// the whole case, while ctx ends when the bring-up does.
		// Bound to the attempt it was torn down the instant the attempt
		// succeeded, so every member came up into a mesh whose relays
		// were already cut — measured as
		// TestAPartitionedMemberIsSilentRatherThanSlow failing all four
		// attempts on "routed to [] (want 1 peers)". The values are
		// inherited; only the cancellation is not.
		if err := f.start(context.WithoutCancel(ctx)); err != nil {
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
		cfg := memberConfig(base, name, i, n, routePorts[i], routes)
		// Loopback rather than every interface: the relays dial
		// 127.0.0.1, and a member listening wider would be reachable
		// from outside the harness on a port a partition does not cut.
		cfg.ClusterHost = "127.0.0.1"
		cfg.ClusterAdvertise = hostPort(dead[i])
		if err := c.start(ctx, t, cfg, i); err != nil {
			return c, err
		}
	}
	return c, c.awaitReady(ctx)
}

// Client connects a queue to member i. Each engine node in a fleet talks to
// the member embedded in its own process, which is what this models.
func (c *Cluster) Client(t testing.TB, i int) *js.Queue {
	t.Helper()
	q, err := c.Servers[i].Client(t.Context())
	if err != nil {
		t.Fatalf("cluster member %d: client: %v", i, err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	return q
}

// StopMember stops member i as its process dying would: the server shut down
// under whatever it held. Its peers go on dialling its route port, and
// [Cluster.RestartMember] brings it back there. Stop every client of it
// first — a client of a member that is gone reconnects to nothing.
func (c *Cluster) StopMember(i int) {
	c.Servers[i].Shutdown()
}

// RestartMember brings member i back after [Cluster.StopMember] and answers a
// client of it, which stops with the cluster. t reports.
//
// THE SAME MEMBER, never a new one: its own store directory, so it rejoins
// holding every replica it held, and its own route port, which is the one
// address its peers know — they have dialled it since the member stopped, and
// nats-server retries an explicit route for ever. So a port somebody else took
// in the window is REPORTED, naming it, and never retried on another, which no
// peer would ever dial: the run that loses that race fails saying why rather
// than standing up a member nobody routes to.
func (c *Cluster) RestartMember(t testing.TB, i int) *js.Queue {
	t.Helper()
	srv := c.restartServer(t, i)
	q, err := srv.Client(t.Context())
	if err != nil {
		t.Fatalf("cluster member %d: a client of the restarted member: %v", i, err)
	}
	c.life.Cleanup(func() { _ = q.Stop(context.Background()) })
	return q
}

// RestartMemberWith is [Cluster.RestartMember] with member i's config changed
// first — another keyring, for a rotation step or a member that holds no key
// its peers do — and the change kept for every later restart of it.
func (c *Cluster) RestartMemberWith(t testing.TB, i int, change func(*js.Config)) *js.Queue {
	t.Helper()
	change(&c.Configs[i])
	return c.RestartMember(t, i)
}

// restartServer is [Cluster.RestartMember]'s server, with no client of it.
func (c *Cluster) restartServer(t testing.TB, i int) *js.Server {
	t.Helper()
	cfg := c.Configs[i]
	switch free, err := PortFree(t.Context(), cfg.ClusterHost, cfg.ClusterPort); {
	case err != nil:
		t.Fatalf("cluster member %d: its route port %d on %q cannot be probed: %v", i,
			cfg.ClusterPort, cfg.ClusterHost, err)
	case !free:
		t.Fatalf("cluster member %d cannot come back: its route port %d — the one its peers dial — "+
			"was taken while it was down", i, cfg.ClusterPort)
	}
	// A PORT LOST BETWEEN THE PROBE AND THE BIND IS THE SERVER'S TO REPORT, as
	// it is for [Cluster.start]: a route listener that cannot bind is a Fatalf
	// in nats-server, which [js.StartServer] refuses as [js.ErrRoutePortTaken]
	// rather than starting a member that serves clients and never routes. So a
	// member StartServer answers is listening on the port its peers dial.
	srv, err := js.StartServer(t.Context(), cfg)
	switch {
	case errors.Is(err, js.ErrRoutePortTaken):
		t.Fatalf("cluster member %d cannot come back: its route port %d — the one its peers dial — "+
			"was taken between the probe and the bind: %v", i, cfg.ClusterPort, err)
	case err != nil:
		t.Fatalf("cluster member %d: restart: %v", i, err)
	}
	// WITH THE CLUSTER, never with the test that restarted it.
	c.life.Cleanup(srv.Shutdown)
	c.Servers[i] = srv
	return srv
}

// harnessRing is the keyring every member this harness starts holds when the
// caller's base gives it none: one key of a literal placeholder material, never
// one anybody deploys. The cases that need another ring — a rotation, a member
// on a foreign one — restart a member onto it ([Cluster.RestartMemberWith]).
var harnessRing = runtoken.OneKey("jetstreamtest", "the jetstreamtest harness's placeholder fleet key material")

// fleetAuth is [harnessRing]'s fleet credentials: what every member presents
// on its routes and accepts on its route and leaf listeners, and what a leaf a
// case joins to one of this harness's clusters presents.
var fleetAuth = sync.OnceValue(func() js.FleetAuth {
	auth, err := js.FleetAuthFrom(harnessRing)
	if err != nil {
		panic(err)
	}
	return auth
})

// memberConfig is member i's config: the caller's base, plus the identity and
// membership every member of an n-node cluster named cluster needs — the
// harness's keyring among them, unless the base brings its own, since every
// route and leaf link is admitted on a key derived from one.
func memberConfig(base js.Config, cluster string, i, n, clusterPort int, routes []string) js.Config {
	cfg := base
	if cfg.FleetAuth.IsZero() {
		cfg.FleetAuth = fleetAuth()
	}
	cfg.ServerName = fmt.Sprintf("crewlet-test-%d", i)
	cfg.ClusterName = cluster
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
// A ROUTE PORT LOST TO SOMETHING ELSE IS THE SERVER'S TO REPORT, and it does:
// a route listener that cannot bind is a Fatalf in nats-server, which
// [js.StartServer] refuses at once as [js.ErrRoutePortTaken]. It used to be
// this function's to find — the listener error went only to the log and the
// member started, served clients and never routed — by comparing the port the
// server bound with the one it was given.
// ctx BOUNDS THIS MEMBER'S START, and is the attempt's rather than the test's:
// see [withFreshPorts] for why a ceiling the start cannot observe bounds
// nothing.
func (c *Cluster) start(ctx context.Context, t testing.TB, cfg js.Config, i int) error {
	t.Helper()
	cfg.StoreDir = brokertest.TempDir(t)
	// SIZED WITH ITS DIRECTORY, for brokertest's reason: a member left to
	// derive its cap takes three quarters of the runner's free space, and a
	// case reserving gibibytes on every member — a log at three replicas
	// lands one on each — then passes or fails on how full the machine's
	// disk is. A base that declares a size of its own keeps it.
	if cfg.StoreMaxBytes == 0 {
		cfg.StoreMaxBytes = brokertest.StoreMaxBytes
	}
	// PROBED IMMEDIATELY BEFORE THE SERVER BINDS IT, because the interval
	// between reserving a port and using it is what got long: this harness
	// reserves twelve at once and then starts n(n-1) relays before the
	// first member asks for one. The probe cannot close the race — nothing
	// can, short of never letting the port go — but it shortens the window
	// from seconds to microseconds, and it finds the loss before the
	// member has spent anything on a start that cannot route.
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
		return fmt.Errorf("cluster member %d: %w", i, startErr(err))
	}
	t.Cleanup(srv.Shutdown)
	c.Servers = append(c.Servers, srv)
	c.Configs = append(c.Configs, cfg)
	return nil
}

// startErr classifies a member's start failure for [withFreshPorts].
//
// A BROKER THAT DECLARED ITSELF DEAD ([js.ErrBrokerDied]) is the one start
// failure known to repeat: what nats-server calls fatal is a fact about the
// configuration or host (an unusable store directory, an unreadable advertise
// address), so retrying only spends the budget re-asking. The exception is a
// listener port somebody else took between reservation and bind, the race the
// attempts exist for, reported as [js.ErrRoutePortTaken] or
// [js.ErrLeafPortTaken] beside the death and retried.
//
// Everything else keeps the default, which is to retry: see [withFreshPorts].
func startErr(err error) error {
	if errors.Is(err, js.ErrBrokerDied) && !errors.Is(err, js.ErrRoutePortTaken) &&
		!errors.Is(err, js.ErrLeafPortTaken) {
		return fmt.Errorf("%w: %w", errNotRetryable, err)
	}
	return err
}

// routeURL is a member's route address as NATS spells it.
func routeURL(port int) string { return "nats://" + hostPort(port) }

// hostPort is a loopback address for a port on this host.
func hostPort(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// PortFree reports whether a port can still be bound right now.
//
// A probe, not a reservation: it finds a lost race before the member recovers
// its store. Exported because a fleet of ENGINES builds servers from Tier A
// (see [Relays]) and cannot call [Cluster.start]. The host is an argument,
// since one assumed can answer about an address the server never binds.
//
// A PROBE THAT COULD NOT ANSWER IS NOT AN ANSWER, so the error comes back
// rather than folding into the bool. [js.PortAvailable]: an occupied port is
// (false, nil); an address this host lacks, a privileged port or a cancelled
// probe is (false, err). Collapsing it made a caller retry a configuration
// mistake and then report a race.
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
func freePorts(ctx context.Context, t testing.TB, n int) []int {
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
