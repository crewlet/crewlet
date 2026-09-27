package e2e

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A FLEET, end to end: n engines, each embedding its own broker, clustered.
//
// # Why this is a second constructor and not a replacement for start
//
// [start] is the shape almost every case here wants, and it is the shape a
// quickstart runs: one node, one broker, no quorum. Making it a degenerate
// cluster would put a route mesh, a metadata group and a replica count under
// every case in this package — three more things that can be slow or wrong,
// under sixty tests whose subject is none of them.
//
// What this adds is the mechanisms a solo node never touches: replication,
// quorum, leader election, placement, seat handover, and the state log's own
// fleet behaviour — a record one node publishes appearing in another node's
// rows, and a node too far behind adopting a peer's snapshot.
//
// # Every route runs through a relay
//
// The members' routes go through per-ordered-pair relays this process owns, so
// a case can cut one member off and put it back. That costs n(n-1) listeners
// and a copy of every route byte, which is why [start] does not pay it — and
// it is the only way to exercise a partition, because stopping a listener
// alone leaves NATS's established routes working indefinitely.
type cluster struct {
	nodes  []*node
	relays *jetstreamtest.Relays
}

// startCluster stands up n nodes, each an engine and its API, on one clustered
// broker.
func startCluster(t *testing.T, n int) *cluster {
	t.Helper()
	if n < 2 {
		t.Fatalf("startCluster(%d): use start(t) for one node", n)
	}
	return startMesh(t, jetstreamtest.StartDirectMesh, n)
}

// startPartitionableCluster is [startCluster] on a mesh whose every route runs
// through a relay this process owns, so a case can cut one member off and put
// it back. Only the cases that partition take it — see
// [jetstreamtest.StartDirectMesh] for what it costs.
func startPartitionableCluster(t *testing.T, n int) *cluster {
	t.Helper()
	return startMesh(t, jetstreamtest.StartRelays, n)
}

// startMesh retries the whole fleet, WITH A FRESH MESH EACH TIME.
//
// A FACTORY rather than a built mesh, and that is the fix rather than a
// refactor. This took the mesh ready-made and reused its port numbers for
// every attempt, while claiming in its own comment to be applying
// [jetstreamtest.ClusterStartAttempts]'s reasoning — which says the opposite
// in as many words: "A wider reservation window would not help, because the
// window is not the problem: the collision is with a process this one does not
// coordinate with. What DOES fix it is noticing and trying again with
// DIFFERENT NUMBERS." Retrying with the same numbers is retrying the question
// somebody else already answered, so a genuinely lost port failed all three
// attempts identically and with nothing to say which of the two causes it was.
func startMesh(t *testing.T, mesh func(context.Context, *testing.T, int) *jetstreamtest.Relays, n int) *cluster {
	t.Helper()
	// BOUNDED IN WALL CLOCK AS WELL AS IN TRIES — see
	// [jetstreamtest.ClusterStartBudget]. Three attempts at an unbounded
	// cost each is a product nobody declared, and on this package it was
	// measured at 1077 seconds across three cases, 64% of the whole run,
	// against a package timeout it came within 115 seconds of firing.
	//
	// AND THE DEADLINE IS HANDED TO THE ATTEMPT rather than only consulted
	// after it. Checked between attempts alone it bounded how MANY were
	// made and nothing about how long one took: buildMember reaches
	// engine.New on the test's own context, whose bring-up may spend
	// [jsprovision.SequenceBudget], so one attempt could outlast this whole
	// ceiling before the loop looked at it. A budget the work cannot
	// observe is a comment rather than a bound.
	deadline := time.Now().Add(jetstreamtest.ClusterStartBudget)
	for attempt := 1; attempt <= clusterStartAttempts; attempt++ {
		// THE MESH IS BUILT INSIDE THE ATTEMPT, because reserving its
		// ports and starting its relays is part of what the ceiling
		// bounds. Built before the context existed it was counted
		// against the budget and bounded by none of it, and
		// [jetstreamtest.StartRelays] carried a second budget of its
		// own: relay setup could spend the whole ceiling and leave the
		// member start an already-expired context, so the loop reported
		// "no cluster came up" having never started a member.
		// THE SOONER OF THE CEILING AND THIS ATTEMPT'S OWN TERM, which is
		// [jetstreamtest.StartAttemptEnd]'s and not a second copy: handed the
		// whole remaining budget, attempt one could spend all of it and the
		// other three never ran — measured, on this case, at 181s of a 180s
		// ceiling with "1 of 4 attempts" in the failure.
		attemptCtx, cancelAttempt := context.WithDeadline(t.Context(),
			jetstreamtest.StartAttemptEnd(time.Now(), deadline))
		relays := mesh(attemptCtx, t, n)
		c, err := startMeshOnce(attemptCtx, t, relays, n)
		cancelAttempt()
		if err == nil {
			return c
		}
		// THE FAILED ATTEMPT'S MESH GOES DOWN NOW, beside the members
		// [startMeshOnce] already stopped: its relay listeners hold the
		// ports, and the next attempt is about to ask for a fresh set.
		relays.Stop()

		// RETRY BY DEFAULT, AND NAME WHAT ACTUALLY FAILED.
		//
		// The default is the safe direction for this harness. Standing
		// up n brokers on a shared machine fails for timing far more
		// often than for anything else — a member that could not form
		// its cluster inside the budget, a lost port, a metadata group
		// that was still electing — and every one of those is what the
		// attempts exist to absorb. Only a failure that is KNOWN to
		// repeat identically is worth ending the test on, because for
		// anything else the cost of guessing wrong is a case that would
		// have passed.
		//
		// Measured by getting it backwards: gating the retry on a lost
		// port alone ended three cluster cases on their first attempt,
		// each on an `ensure stream ...: context deadline exceeded` that
		// a second attempt had always absorbed.
		//
		// What the log must not do is call all of them a port race. That
		// was the other half of the same defect: every attempt reported
		// "lost a race" whatever had happened, so the one diagnostic a
		// reader gets named a cause the run had no evidence for.
		if errors.Is(err, errNotRetryable) {
			t.Fatalf("cluster attempt %d/%d failed for a reason retrying "+
				"cannot fix: %v (relays: %s)",
				attempt, clusterStartAttempts, err, relays.Describe())
		}
		// CHECKED AFTER THE ATTEMPT AND BEFORE THE RETRY LINE, so an
		// expired budget never costs a try already paid for and the
		// last thing a reader sees names the bound.
		if time.Now().After(deadline) {
			t.Fatalf("no cluster came up within %s (%d of %d attempts), last: "+
				"%v (relays: %s)", jetstreamtest.ClusterStartBudget, attempt,
				clusterStartAttempts, err, relays.Describe())
		}
		t.Logf("cluster attempt %d/%d failed, retrying with a fresh mesh: %v "+
			"(relays: %s)", attempt, clusterStartAttempts, err, relays.Describe())
	}
	t.Fatalf("no cluster came up in %d attempts", clusterStartAttempts)
	return nil
}

// errNotRetryable marks a member failure that a fresh mesh cannot change: the
// same input producing the same answer every time.
//
// DELIBERATELY SHORT, and everything not on it is retried. A harness that
// guesses "deterministic" wrongly ends a case that would have passed, while
// guessing "transient" wrongly costs some seconds and a log line — so the
// burden is on proving a failure repeats, not on proving it might not.
var errNotRetryable = errors.New("not fixable by another attempt")

// ctx BOUNDS THIS ATTEMPT, and is not the test's own: see [startMesh] for why
// a ceiling the member starts cannot observe bounds nothing.
func startMeshOnce(ctx context.Context, t *testing.T, relays *jetstreamtest.Relays, n int) (*cluster, error) {
	t.Helper()
	c := &cluster{relays: relays, nodes: make([]*node, n)}

	// CONCURRENTLY, which is what a fleet actually does: n machines boot
	// independently. Sequentially is not merely slower — it does not work.
	// A clustered member does not finish starting until its JetStream has
	// caught up with the metadata group, and that group needs a majority,
	// so the first member would wait out its whole readiness budget for
	// peers this loop has not started yet and then fail naming a cluster
	// nobody could have formed.
	errs := make([]error, n)
	stops := make([][]func(), n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// t.Fatalf is the test goroutine's alone, so a member's
			// failure is carried back rather than raised here — a
			// FailNow from another goroutine ends that goroutine and
			// leaves the test running with a nil member.
			c.nodes[i], stops[i], errs[i] = buildMember(ctx, t, relays, i, n)
		}()
	}
	wg.Wait()

	failed := -1
	for i, err := range errs {
		if err != nil && failed < 0 {
			failed = i
		}
	}
	if failed < 0 {
		// THE SUCCESSFUL ATTEMPT'S TEARDOWN IS THE TEST'S, so members
		// live for the case that asked for them.
		t.Cleanup(func() { stopAll(stops) })
		return c, nil
	}

	// EVERY MEMBER THIS ATTEMPT STARTED IS STOPPED BEFORE THE NEXT ONE,
	// and it is the difference between a retry and a pile-up.
	//
	// A member left running is one whose engine is still applying, still
	// heartbeating and still competing for the same cores as the attempt
	// that replaces it — and whose broker is still gossiping under the
	// cluster name the next attempt's members will use, so any member that
	// does come up forms a cluster with the ghost. The observed shape was
	// attempt 1 timing out on one member, attempt 2 failing to become ready
	// at all, and attempt 3 failing worse: not slowness, but each attempt
	// racing everything the last one left.
	//
	// t.Cleanup cannot do this. It runs when the TEST ends, which is after
	// every attempt — so registering teardown there is registering it for
	// the wrong moment.
	stopAll(stops)
	// THE ERROR GOES BACK rather than being judged here: whether it is
	// worth another attempt is the caller's question, and it is the only
	// frame that knows how many are left.
	return nil, fmt.Errorf("member %d: %w", failed, errs[failed])
}

// stopAll runs every member's teardown, in reverse order within each member.
//
// REVERSE, because that is the order the pieces were built in and each one's
// stop assumes the ones after it are still there: the projector reads the
// queue the engine owns, and the server serves the app.
func stopAll(stops [][]func()) {
	for _, member := range stops {
		stopInReverse(member)
	}
}

// clusterStartAttempts is how many times a fleet is stood up before the case
// gives up.
//
// [jetstreamtest.ClusterStartAttempts] ITSELF, not a second number applying
// its reasoning. This used to restate that constant's argument as its own
// while carrying three where the constant carried four — one decision, two
// spellings, each comment asserting it matched the other. That is the shape textcut, whsec and jsprovision were each written to
// remove, and the fix is the same one: read the constant rather than restate
// the argument.
//
// The reasoning it now simply defers to: the collision is with work this
// process does not coordinate with, so a wider window does not help and
// noticing does — and each attempt reserves its own mesh, which is the half
// that makes "trying again" mean something. See [startMesh].
const clusterStartAttempts = jetstreamtest.ClusterStartAttempts

// startMember brings up one member of the fleet.
//
// EVERY NODE HAS ITS OWN STORE AND ITS OWN STREAM DIRECTORY, because that is
// what a fleet is: n machines, each with its own disk. Sharing either would
// make this one node wearing three hats, and every fleet mechanism under it
// would pass for the wrong reason.
// clusterHost is the interface every member of a test mesh binds its route
// listener on. LOOPBACK, because the mesh's own route URLs are loopback and a
// member listening wider would be reachable from outside the harness — and
// because the pre-bind probe asks about one address, which cannot answer for a
// wildcard bind.
const clusterHost = "127.0.0.1"

// ctx is the ATTEMPT's, so the retry loop's wall-clock ceiling can interrupt a
// bring-up rather than only refuse the next one — see [startMesh].
func buildMember(ctx context.Context, t *testing.T, relays *jetstreamtest.Relays, i, n int) (
	*node, []func(), error) {

	// THE TEARDOWN IS RETURNED, NOT REGISTERED WITH t.Cleanup, because an
	// attempt that fails has to stop what it started BEFORE the next one
	// starts — see [startMeshOnce]. It is built up as each piece comes up,
	// so a member that fails halfway still hands back a way to undo the
	// half that worked.
	var stops []func()
	fail := func(err error) (*node, []func(), error) { return nil, stops, err }

	model := newScriptedModel(t)
	// THE MODEL SERVER IS THIS ATTEMPT'S, not the test's. Its own
	// constructor registers a t.Cleanup as well, which is right for the
	// single-node cases — but a cluster case that retries three times would
	// otherwise leave one live server per member per failed attempt running
	// until the case ends. Close is idempotent, so both fire safely.
	stops = append(stops, model.close)
	cfg, err := config.ParseCompany([]byte(fmt.Sprintf(companyDoc, model.url)))
	if err != nil {
		// THE SAME FILE PARSES THE SAME WAY on every attempt.
		return fail(fmt.Errorf("%w: company config: %w", errNotRetryable, err))
	}

	port, routes, advertise := relays.Member(i)
	peers, err := otherMembers(routes, port)
	if err != nil {
		return fail(fmt.Errorf("%w: member %d's routes: %w", errNotRetryable, i, err))
	}
	// PROBED IMMEDIATELY BEFORE THE ENGINE BINDS IT, which is the guard
	// [jetstreamtest.Cluster.start] has and this path did not.
	//
	// It cannot close the race — nothing can, short of never letting the
	// port go — but it shortens the window from however long engine.New
	// takes to get there down to microseconds, and it names the CAUSE. Its
	// partner is the engine's own post-bind check, which is what catches a
	// port lost inside that window: together they turn a member that comes
	// up, serves clients and silently never routes into an immediate,
	// named retry.
	//
	// THE SAME HOST THE MEMBER BINDS, set below — a probe against a
	// different address answers about a port the server never asks for.
	switch free, err := jetstreamtest.PortFree(ctx, clusterHost, port); {
	case err != nil:
		// NOT A RACE: an address this host does not have, or a probe
		// that never ran. Retrying it would spend every attempt on a
		// mistake that answers the same way each time.
		return fail(fmt.Errorf("%w: route port %d on %q cannot be probed for "+
			"member %d: %w", errNotRetryable, port, clusterHost, i, err))
	case !free:
		return fail(fmt.Errorf("%w — route port %d went between this mesh "+
			"reserving it and member %d starting",
			jetstream.ErrRoutePortTaken, port, i))
	}
	boot := config.DefaultBootstrap()
	boot.Node.ID = fmt.Sprintf("node-%d", i)
	boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	boot.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	boot.Stream.Cluster.Name = jetstreamtest.RelayClusterName
	boot.Stream.Cluster.Port = port
	// THE OTHER MEMBERS, which is what Tier A says the field holds and what
	// its member count is taken from — see [otherMembers].
	boot.Stream.Cluster.Peers = peers
	// LOOPBACK, because the relays dial 127.0.0.1: a member listening on
	// every interface would be reachable on a port a partition does not
	// cut, and the partition would cut nothing.
	boot.Stream.Cluster.Host = clusterHost
	if advertise != "" {
		boot.Stream.Cluster.Advertise = advertise
	}
	// QUORUM-DURABLE, which is what makes "a write one node acknowledged
	// is a write every node will see" true rather than aspirational. At
	// one replica a publish is durable on the member that took it, and a
	// case asserting a peer sees it would be asserting timing.
	boot.Stream.Replicas = n
	// ONE LEASE STORE FOR THE FLEET, which is what makes it a fleet rather
	// than n nodes that happen to share a log. The default, `local`, keeps
	// every lease in this process — Tier A refuses it on a clustered stream
	// for exactly that reason — and under it each member claimed every seat
	// and every duty for itself and saw no peer's presence at all. The
	// object store is where that stopped being invisible: its membership is
	// a lease, so each member's map duty saw only its OWN objects lease and
	// counted every other member absent on every tick, and a member could
	// never be taken out, because the ones it would leave were always absent.
	//
	// It went unnoticed because nothing between this struct and the broker
	// held it to Tier A: the loader validates a FILE, and this bootstrap is
	// built in code. engine.New validates what it is given now, so this
	// harness can only stand up a fleet an operator could run — which is
	// also why [fleetSize] is three.
	boot.Coordination.Type = config.CoordinationEmbeddedKV
	// A TOKEN, because a write over HTTP is attributed to the operator it
	// names and a member with none refuses every one — the file uploads
	// cross the fleet through the API.
	boot.API.Auth.Tokens = []config.APIToken{{ID: e2eOperatorID, Token: e2eOperatorToken}}

	e, err := engine.New(ctx, engine.Options{
		Bootstrap: &boot, Company: cfg, ActivatedAt: harnessActivation,
	})
	if err != nil {
		// A CONFIG THE ENGINE REFUSED is refused identically on every
		// attempt — the same file parses the same way — so a fresh mesh
		// would spend the whole start budget re-asking a question already
		// answered, and then report it as a cluster that never came up.
		var fault *config.Fault
		if errors.As(err, &fault) {
			return fail(fmt.Errorf("%w: engine.New: %w", errNotRetryable, err))
		}
		return fail(fmt.Errorf("engine.New: %w", err))
	}
	// ON WithoutCancel, like every teardown here: the attempt's context is
	// cancelled the moment the attempt ends, and a stop that inherited it
	// would be handed a dead context exactly when it has work to do — the
	// rule internal/engine states for a rollback, applied to a harness.
	stops = append(stops, func() { e.Stop(context.WithoutCancel(ctx)) })
	if err := e.Start(ctx); err != nil {
		return fail(fmt.Errorf("engine.Start: %w", err))
	}

	// THROUGH wireAPI, NOT serveAPI: this runs off the test's goroutine,
	// where t.Fatalf would end only this goroutine, and the API's teardown
	// belongs in this member's list, after the engine's, so a failed
	// attempt stops the listener, the projector and the app before the
	// engine they read from, and before the next attempt starts.
	//
	// THE SUPPRESSION BELOW: the context handed over is deliberately NOT
	// this attempt's. `ctx` is cancelled the moment the bring-up ends, and
	// what wireAPI starts under it — the app's push ticks and the live
	// projector — serves for the whole case. The test's own context is
	// that lifetime exactly: longer than the attempt, and still ended when
	// the case is over, which [context.WithoutCancel] would not be.
	app, srv, apiStops, err := wireAPI(t.Context(), e, &boot, nil) //nolint:contextcheck // see above
	stops = append(stops, apiStops...)
	if err != nil {
		return fail(fmt.Errorf("api: %w", err))
	}

	return &node{
		engine: e, app: app, server: srv, model: model,
		id:          boot.Node.ID,
		snapshotDir: boot.Store.SnapshotDirFor(),
	}, stops, nil
}

// otherMembers is a mesh's route list without this member's own route.
//
// `stream.cluster.peers` is "the route URLs of the OTHER members", and Tier A
// counts a fleet as those plus this node — which is where its two-member
// refusal is decided. The direct mesh hands every member the same list, its
// own route included (NATS ignores a route to itself, so the broker runs the
// same either way), and handed through unfiltered that list counted a fleet
// of two as three: the two-member harness passed the very rule that exists to
// refuse it, on every case but the partitioned one, whose relay mesh lists
// only the others. Filtered here, so the count Tier A makes is the fleet this
// harness actually stands up.
func otherMembers(routes []string, own int) ([]string, error) {
	var out []string
	for _, route := range routes {
		u, err := url.Parse(route)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", route, err)
		}
		if u.Hostname() == clusterHost && u.Port() == strconv.Itoa(own) {
			continue
		}
		out = append(out, route)
	}
	return out, nil
}

// hydrated waits for every member's replication loops to catch up.
//
// A CLUSTER, NOT A NODE: a case that waited on one member and then read
// another is asserting how fast a route is, which is the one thing a test on
// a loaded machine cannot rely on.
func (c *cluster) hydrated(t *testing.T) {
	t.Helper()
	for i, n := range c.nodes {
		waitFor(t, fmt.Sprintf("member %d's native backends to hydrate", i),
			hydrated(t, n.engine))
	}
}

// noParallel is what a cluster case calls INSTEAD of [testing.T.Parallel], and
// the omission is deliberate rather than an oversight.
//
// A three-member cluster in this package is three embedded NATS servers, three
// pairs of SQLite databases and better than twenty raft groups, all on one
// machine. Two of them at once is twice that competing for one disk and one
// scheduler, and what gives way first is raft: measured here, each case passes
// alone in under forty seconds and both fail together — a member's metadata
// read timing out after thirty, reported as a boot failure naming a stream.
//
// A function rather than a bare comment because the absence of a call is not
// something a reader notices, and "why is this one not parallel" is exactly
// the question a later edit answers by adding it back.
func noParallel(t *testing.T) {
	t.Helper()
	t.Setenv("CREWLET_CLUSTER_CASE", "1") // Setenv also forbids t.Parallel.
}

// fleetSize is how many members a cluster case stands up.
//
// THREE, because that is the smallest fleet Tier A will run. Two embedded
// members on the embedded KV are refused by name (`stream.cluster.peers`, in
// [config.Bootstrap.Validate]): two have no coordination quorum without each
// other, so the fleet stops serving the moment either restarts, and a rolling
// upgrade restarts them one at a time. That rule is right for production, and
// engine.New now holds every bootstrap to it — so a harness of two was a
// harness running a fleet no operator could, which is what this had been twice
// over: first on local coordination, where every member leased every seat to
// itself, and then on the KV at a size Tier A refuses.
//
// IT WAS TWO FOR COST, measured when this package shared a runner with the
// rest of the tree: three embedded brokers with better than twenty raft groups
// each starved each other, each case passing alone and every one timing out
// under `go test ./...`. That contention is gone rather than paid for here —
// this is a solo package ([solo.Run], in logsink_test.go), run at -p 1 with no
// other package beside it, and no two of its cluster cases ever run at once
// ([noParallel]). What the third member costs now, measured on a four-core
// machine under -race, the ten cluster cases run alone and in sequence: 494 s
// at three members in each of two runs against 476 s at two, about 4% — the
// fixed cost of a case is standing a fleet up and tearing it down, and the
// extra member stands up beside the others rather than after them. A third
// run took 584 s, all of the difference one cluster start that timed out and
// was retried, which is what [clusterStartAttempts] is for. (The search case
// got cheaper, 60 s to 34–50 s, because it now writes the handful of pages
// that cover the table rather than two dozen.)
//
// WHAT IT BUYS beyond being runnable is a MAJORITY: the partition case cuts one
// member off while the other two keep their quorum, so the scatter has to tell
// an absent peer from a present one rather than from nobody at all.
const fleetSize = 3

// clusterSettle is how long a fleet assertion waits for a record one member
// published to reach another's rows.
//
// It is NOT a guess about the network. The path is publish → quorum → the
// peer's own durable consumer → its applier's next batch, and every one of
// those is bounded except the last, which runs on the applier's own loop. Ten
// seconds is roughly forty times the measured path on an idle machine and
// still finite, so a case that hangs fails with a message rather than with a
// suite timeout naming nothing.
const clusterSettle = 10 * time.Second

// A RECORD ONE NODE WRITES REACHES EVERY NODE'S ROWS.
//
// This is the whole claim of the state log stated as a product fact, and it is
// the one a single-node suite cannot make. What it exercises is the entire
// path: a write arbitrated at the broker on the task's own subject, replicated
// to quorum, delivered to each node's own durable consumer, and applied into
// each node's own SQL with the checkpoint committed alongside the rows.
//
// The failure it protects against is not "the record was lost". It is the
// quieter one: two nodes' boards disagreeing about the same company, which
// from either screen looks exactly like the other node being idle.
func TestARecordOneNodeWritesReachesEveryNodesRows(t *testing.T) {
	noParallel(t)
	c := startCluster(t, fleetSize)
	c.hydrated(t)

	// THE CHART FIRST. A create takes its key from its project's own
	// counter, so the project has to exist — every node applies the chart
	// at boot, and this is also the first assertion that it did.
	task := newTask("ENG", "the fleet-wide one")
	written, err := operator(t, c.nodes[0]).CreateTask(
		t.Context(), "cluster-create-1", task, nil)
	if err != nil {
		t.Fatalf("create on member 0: %v", err)
	}
	if written.Outcome == statelog.OutcomeUnknown {
		t.Fatalf("the create resolved to %q on the node that made it", written.Outcome)
	}

	for i, n := range c.nodes {
		// WAITED FOR PER NODE rather than asserted at once: the members
		// apply independently, so "member 2 has it" is a fact that
		// becomes true at its own moment, and a single read of all
		// three would be asserting how fast a route is.
		deadline := time.Now().Add(clusterSettle)
		var last error
		for time.Now().Before(deadline) {
			detail, err := n.engine.Tracker().Task(t.Context(), written.Key,
				tracker.DetailWants{}, statelog.Freshness{Level: statelog.ReadSession})
			if err == nil && detail.Task.Title == task.Title {
				last = nil
				break
			}
			last = err
			time.Sleep(50 * time.Millisecond)
		}
		if last != nil {
			t.Errorf("member %d never applied %s (%v) — two nodes' boards "+
				"disagree about one company, which from either screen looks "+
				"like the other node being idle", i, written.Key, last)
		}
	}
}

// EVERY NODE MINTS INTO ONE KEY SPACE, and no two tasks share a key.
//
// # Why this is the case a fleet has and a solo node does not
//
// A key is what people paste into chat, so two tasks sharing one is a person
// opening the wrong record from somebody's link. The counter that mints them
// is arbitrated on the PROJECT's own subject: three nodes minting at once
// contend at the broker and exactly one wins each round, and the losers read
// the value that won and take the next.
//
// On one node that arbitration is untested — there is nobody to lose to. Here
// all three file into one project simultaneously, which is exactly the shape
// that produced two ENG-1s in the design this replaced.
func TestEveryNodeMintsIntoOneKeySpace(t *testing.T) {
	noParallel(t)
	c := startCluster(t, fleetSize)
	c.hydrated(t)

	type filed struct {
		key string
		err error
	}
	results := make(chan filed, len(c.nodes))
	for i, n := range c.nodes {
		go func() {
			// RETRIED ON `behind`, which is what a real caller does and
			// what makes this case about the KEY SPACE rather than
			// about timing. A node whose applier has not caught up
			// with the counter refuses rather than deciding from a
			// state below it — that refusal is the design working, and
			// the operation id is stable across the attempts so a
			// retry after a copy that landed collapses rather than
			// filing twice.
			opID := fmt.Sprintf("cluster-mint-%d", i)
			task := newTask("ENG", fmt.Sprintf("filed by member %d", i))
			var got tracker.WriteResult
			var err error
			deadline := time.Now().Add(clusterSettle)
			for time.Now().Before(deadline) {
				got, err = operator(t, n).CreateTask(t.Context(), opID, task, nil)
				if err == nil || !errors.Is(err, statelog.ErrUnavailable) {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			results <- filed{key: got.Key, err: err}
		}()
	}
	seen := map[string]bool{}
	for range c.nodes {
		got := <-results
		if got.err != nil {
			t.Errorf("a concurrent create never landed: %v", got.err)
			continue
		}
		if seen[got.key] {
			t.Errorf("two tasks were minted the key %s — a person following "+
				"somebody's link opens the wrong record", got.key)
		}
		seen[got.key] = true
	}
	if len(seen) != len(c.nodes) && !t.Failed() {
		t.Errorf("%d concurrent creates produced %d keys: %v", len(c.nodes), len(seen), seen)
	}
}

// A NODE TAKES SNAPSHOTS AND SERVES THEM, which is the half of the recovery
// path a recipient cannot supply for itself.
//
// # Why this is asserted as a product fact rather than a unit
//
// [statelog] certifies the snapshot loop and the transfer against fakes. What
// it cannot certify is that a running ENGINE arms them — and half-wired is the
// worst state this mechanism has, because the wired half reports itself
// working: a node below the log's floor asks the fleet, nothing answers, and
// it comes up on the history it has for ever with one line in its log.
//
// So the assertion is on the artefact: a member of a running fleet has one on
// disk, its manifest names every registered domain, and a peer asking the
// fleet is offered it. Nothing here forces a node below the floor — that needs
// a trim, which needs a backup, which is [internal/backup]'s own suite. What
// this establishes is that if one ever did fall behind, there would be
// something to adopt.
func TestAFleetTakesAndOffersSnapshots(t *testing.T) {
	noParallel(t)
	c := startCluster(t, fleetSize)
	c.hydrated(t)

	// A SNAPSHOT IS TAKEN AT BOOT, not only on the interval — the
	// interval is measured against the newest artefact on DISK, so a node
	// restarted more often than it would otherwise never take one.
	// ANY MEMBER'S, because the claim is the FLEET's: a snapshot exists to
	// be adopted from, and which member happens to hold the newest one is
	// a scheduling detail. Reading only member 0 would make this case fail
	// whenever that member skipped a tick for a reason the design allows.
	var manifest statelog.Manifest
	waitFor(t, "a member of the fleet to have taken a snapshot", func() bool {
		for i := range c.nodes {
			if m, found := newestSnapshotIn(c.dir(t, i)); found {
				manifest = m
				return true
			}
		}
		return false
	}, func() string {
		var dirs []string
		for i := range c.nodes {
			dirs = append(dirs, c.dir(t, i))
		}
		return "snapshot dirs: " + strings.Join(dirs, ", ")
	})

	// EVERY REGISTERED DOMAIN OR NOTHING: a recipient refuses an artefact
	// that does not name one of its own domains, so a snapshot missing a
	// domain is one nobody can adopt — and it is indistinguishable from a
	// healthy one until somebody needs it.
	for _, want := range []string{"tracker", "vectors", "pages"} {
		if _, named := manifest.Domains[want]; !named {
			t.Errorf("the snapshot names %v and not %q — a recipient refuses "+
				"an artefact that does not name every domain it registers",
				slices.Sorted(maps.Keys(manifest.Domains)), want)
		}
	}
	if manifest.SHA256 == "" {
		t.Error("the snapshot carries no checksum, so a recipient cannot tell " +
			"a complete transfer from a truncated one")
	}

	// AND A PEER ASKING THE FLEET IS OFFERED IT, which is the donor half
	// — the one that has to be running in somebody's process for the
	// recipient's half to ever complete.
	conn := c.conn(t, 1)
	need := map[string]uint64{}
	gens := map[string]uint32{}
	for name := range manifest.Domains {
		need[name], gens[name] = 0, 0
	}
	offers, err := statelog.CollectOffers(t.Context(), conn, statelog.OfferRequest{
		NodeID: "a-joining-node", Need: need, Generations: gens,
	}, 3*time.Second)
	if err != nil {
		t.Fatalf("collect offers: %v", err)
	}
	if len(offers) == 0 {
		t.Fatalf("a fleet of %d offered a joining node nothing — the donor "+
			"half is not running, so a node below the log's floor could never "+
			"adopt and would come up on the history it has for ever", fleetSize)
	}

	// AND THE REGISTER SAYS SO, which is the half a file on disk cannot
	// establish: the trim's snapshot term counts donors from the POSITIONS
	// REGISTER, never from anybody's directory, and it refuses to remove
	// anything until two counted nodes hold a verified artefact. A fleet
	// that takes snapshots and publishes none of them therefore never
	// trims any domain for the life of the deployment — with the applied
	// term, the backup term and every operator surface reporting a
	// perfectly healthy fleet.
	fleet := c.nodes[0].engine.Backends().Fleet
	var donors int
	waitFor(t, "the fleet to publish its snapshots into the position register",
		func() bool {
			rows, err := fleet.Positions(t.Context())
			if err != nil {
				return false
			}
			donors = 0
			for _, row := range rows {
				if row.SnapshotBytes == 0 {
					continue
				}
				// EVERY DOMAIN OR NONE, which is the artefact's
				// own shape: one file covers all of them, so a
				// row naming some is a row assembled per domain
				// from something other than a manifest.
				covered := true
				for _, at := range row.Domains {
					if at.SnapshotAt.IsZero() ||
						at.SnapshotGeneration != at.Generation {
						covered = false
					}
				}
				if covered {
					donors++
				}
			}
			return donors >= statelog.SnapshotDonorsRequired
		}, func() string {
			rows, err := fleet.Positions(t.Context())
			if err != nil {
				return "positions unreadable: " + err.Error()
			}
			var out []string
			for _, row := range rows {
				out = append(out, fmt.Sprintf("%s bytes=%d skip=%q domains=%v",
					row.NodeID, row.SnapshotBytes, row.SnapshotSkip, row.Domains))
			}
			return strings.Join(out, " | ")
		})
}

// indexedShards is the pages one member's lexical index holds, by id, with the
// bucket each was filed in.
//
// The index is the NODE's own estate, which no typed store exposes a listing
// of, so this reads its table directly — the same shape [digestTable] takes
// for the replicated rows.
func indexedShards(t *testing.T, n *node) map[string]int {
	t.Helper()
	out := map[string]int{}
	if err := n.engine.Backends().Store.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT source_id, search_shard FROM kb_docs WHERE source = ?`,
			string(search.SourcePage))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			var shard int
			if err := rows.Scan(&id, &shard); err != nil {
				return err
			}
			out[id] = shard
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read %s's lexical index: %v", n.id, err)
	}
	return out
}

// dir is member i's snapshot directory.
func (c *cluster) dir(t *testing.T, i int) string {
	t.Helper()
	return c.nodes[i].snapshotDir
}

// conn is member i's own broker connection.
func (c *cluster) conn(t *testing.T, i int) *nats.Conn {
	t.Helper()
	q, ok := c.nodes[i].engine.Backends().Queue.(interface{ Conn() *nats.Conn })
	if !ok || q.Conn() == nil {
		t.Fatalf("member %d has no broker connection", i)
	}
	return q.Conn()
}

// newestSnapshotIn is the newest complete manifest in a directory, on the
// engine's own terms: the manifest is written last, so an entry without one is
// debris rather than a snapshot.
func newestSnapshotIn(dir string) (statelog.Manifest, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return statelog.Manifest{}, false
	}
	var newest statelog.Manifest
	var found bool
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		m, err := statelog.ReadManifest(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		if !found || m.TakenAt.After(newest.TakenAt) {
			newest, found = m, true
		}
	}
	return newest, found
}

// nodeIDs is what the members call themselves, which is their name in a
// search fan-out's assignment table and on their own presence leases.
func (c *cluster) nodeIDs() []string {
	out := make([]string, 0, len(c.nodes))
	for _, n := range c.nodes {
		out = append(out, n.id)
	}
	return out
}

// THE FLEET ANSWERS ONE SEARCH BETWEEN ITS MEMBERS.
//
// # What this adds over the coordinator's own suite
//
// [internal/search]'s cases drive the fan-out over fixtures: they prove the
// merge, the coverage arithmetic and the wire format. What none of them proves
// is that REAL engines — each with its own broker, its own store, its own
// index and its own registration — answer each other's slice requests. This is
// the only place the subject, the answerer, the assignment table and
// independently built indexes are all live at once, and it is exactly the arm
// a single-node suite passes vacuously.
//
// # Why the assertion is on the COVERAGE and not only on the hits
//
// Every member holds the whole corpus, so none NEEDS another to answer. A
// fan-out that silently fell back to the local scan would return the same
// documents in the same order — what separates the two is which buckets each
// answer came from, which is what this reads.
//
// # And why the table is explicit rather than derived from the corpus size
//
// A search below [search.FanOutFloor] is answered by the asking node alone, by
// design, and writing ten thousand pages through the real write path to cross
// that floor would measure the applier rather than the fan-out. The floor's own
// decision is [search]'s to test; what a fleet can see is the network, so this
// case hands out the table the floor would have produced.
func TestTheFleetAnswersOneSearchBetweenItsMembers(t *testing.T) {
	noParallel(t)
	c := startCluster(t, fleetSize)
	c.hydrated(t)

	table := search.Divide(c.nodeIDs())
	if len(table) != fleetSize {
		t.Fatalf("a fleet of %d divided into %d assignments", fleetSize, len(table))
	}

	// PAGES UNTIL EVERY MEMBER'S RANGE HOLDS ONE, and no more. A page's
	// bucket is a hash of the id its write mints, so a fixed count is a
	// probability rather than a coverage: the 24 pages this wrote left one
	// of two halves empty about once in four million runs, and would leave
	// one of three ranges empty about once in five thousand — a flake nobody
	// could reproduce. Each page is a real write through a quorum cluster,
	// so writing until the table is covered is also the cheaper shape: a
	// handful of pages rather than two dozen. The bound is one page per
	// bucket, past which a range still empty is a hash or a table that does
	// not partition the buckets, and fails naming itself rather than looping.
	writer := c.nodes[0].engine.PagesStore()
	var last statelog.Position
	covered := map[string]int{}
	shards := map[string]int{}
	written := 0
	for ; len(covered) < len(table); written++ {
		if written == search.SearchShards {
			t.Fatalf("%d pages reached the ranges of %d of %d members (%v) — "+
				"the table does not partition the buckets the pages hash to",
				written, len(covered), len(table), covered)
		}
		page, err := writer.Create(t.Context(), pageOperator(), pages.NewPage{
			Container: "ENG",
			Title:     fmt.Sprintf("Runbook %02d", written),
			Body:      "when a deploy hangs on rollback, drain the node before retrying",
		})
		if err != nil {
			t.Fatalf("create page %d: %v", written, err)
		}
		last = page.Outcome.Position
		shard := search.ShardOf(string(search.SourcePage), page.Page.ID)
		shards[page.Page.ID] = shard
		for _, a := range table {
			if a.Shards.Contains(shard) {
				covered[a.Node]++
			}
		}
	}
	for i, n := range c.nodes {
		if err := n.engine.WaitCommitted(t.Context(), last); err != nil {
			t.Fatalf("member %d never applied the corpus: %v", i, err)
		}
		searcher := n.engine.NativeSearcher()
		if searcher == nil {
			t.Fatalf("member %d runs no native searcher", i)
		}
		waitFor(t, fmt.Sprintf("member %d's first index build", i), func() bool {
			return !searcher.Building(t.Context())
		})
		// AND EVERY PAGE WRITTEN IN IT, which Building does not say: it
		// answers whether the FIRST build finished ([search.Indexer.Ready]),
		// and an index a page behind is ordinary staleness to a searcher.
		// Here it would be a peer answering nothing for a range whose page
		// it has not indexed yet — and the page that completes the coverage
		// above is the LAST one written, so the range it lands in is the one
		// most likely to hold nothing else. Read off the index's own rows,
		// with the bucket each was filed in, so the coverage the loop above
		// computed is checked against the bucket the index actually used.
		var indexed map[string]int
		waitFor(t, fmt.Sprintf("member %d's index to hold all %d pages", i, written),
			func() bool {
				indexed = indexedShards(t, n)
				for id := range shards {
					if _, ok := indexed[id]; !ok {
						return false
					}
				}
				return true
			}, func() string { return fmt.Sprintf("indexed %v, written %v", indexed, shards) })
		for id, shard := range shards {
			if indexed[id] != shard {
				t.Fatalf("member %d filed page %s in bucket %d and the coverage "+
					"was computed for bucket %d", i, id, indexed[id], shard)
			}
		}
	}

	// EVERY MEMBER ASKS, because a coordinator is whichever node the
	// search happened to reach — there is no leader here, and a fan-out
	// that only worked from member 0 would be a fleet with one search node
	// and no way to tell.
	for i, n := range c.nodes {
		self := n.id
		var peers []search.Assigned
		var mine search.Assignment
		for _, a := range table {
			if a.Node == self {
				mine = a.Shards
				continue
			}
			peers = append(peers, a)
		}

		deadline, cancel := context.WithTimeout(t.Context(), clusterSettle)
		answers, err := search.Broker{Queue: n.engine.Backends().Queue}.Scatter(
			deadline, search.FanQuery{
				Text: "rollback drain node", Sources: []string{"page"},
			}, peers)
		cancel()
		if err != nil {
			t.Fatalf("member %d could not scatter: %v", i, err)
		}
		if len(answers) != len(peers) {
			t.Fatalf("member %d asked %d peers and %d answered — a peer that "+
				"holds the whole corpus did not answer the range it was given",
				i, len(peers), len(answers))
		}
		for _, slice := range answers {
			if slice.Node == self {
				t.Errorf("member %d answered its own scatter, so its range is "+
					"scanned twice and its documents merged against themselves", i)
			}
			if len(slice.Lexical) == 0 {
				t.Errorf("member %d's peer %s returned nothing for buckets "+
					"[%d,%d), which hold %d of the %d pages written, all of "+
					"which match", i, slice.Node, slice.Shards.From,
					slice.Shards.To, covered[slice.Node], written)
			}
		}

		// AND THE ANSWERS PARTITION THE CORPUS. A document two peers both
		// returned is counted twice by the merge and ranked above where it
		// belongs; one in the asker's own range that a peer also returned
		// is the same failure from the other side.
		seen := map[string]int{}
		for _, slice := range answers {
			for _, hit := range slice.Lexical {
				seen[hit.Key]++
			}
		}
		for key, times := range seen {
			if times != 1 {
				t.Errorf("member %d: %s was returned by %d peers", i, key, times)
			}
			source, id, _ := strings.Cut(key, ":")
			if shard := search.ShardOf(source, id); mine.Contains(shard) {
				t.Errorf("member %d holds bucket %d and a peer returned %s "+
					"from it", i, shard, key)
			}
		}
	}
}

// WHAT A FLEET GUARANTEES A READER, AND THAT ITS MEMBERS AGREE.
//
// Three claims that only a fleet can make, in one case because they need one
// fleet: standing three of these up costs three embedded brokers and six
// databases, and the arms do not interfere.
//
//  1. A LINEARIZABLE READ ON ANOTHER MEMBER SEES AN ACKNOWLEDGED WRITE, with
//     no wait loop. That is what the barrier append buys and the one thing
//     [TestARecordOneNodeWritesReachesEveryNodesRows] deliberately does not
//     assert — it polls at session level, because its subject is that the
//     record arrives at all rather than when.
//  2. THE MEMBERS ARE TWINS. Every domain is N identical SQL copies, so two
//     members answering differently about one company is the failure the whole
//     framework exists to make impossible — and from either screen it looks
//     exactly like the other node being idle.
//  3. THE KNOWLEDGE BASE IS ONE OF THOSE DOMAINS TOO. Pages arrived on the log
//     later than the tracker did, and the arm that would have caught a
//     half-adopted domain is this one.
func TestAFleetAgreesAboutOneCompany(t *testing.T) {
	noParallel(t)
	c := startCluster(t, fleetSize)
	c.hydrated(t)

	written, err := operator(t, c.nodes[0]).CreateTask(t.Context(),
		"fleet-agrees-1", newTask("ENG", "the linearizable one"), nil)
	if err != nil {
		t.Fatalf("create on member 0: %v", err)
	}
	if written.Outcome == statelog.OutcomeUnknown {
		t.Fatalf("the create resolved to %q on the node that made it",
			written.Outcome)
	}
	page, err := c.nodes[0].engine.PagesStore().Create(t.Context(), pageOperator(),
		pages.NewPage{
			Container: "ENG", Title: "Fleet rollback runbook",
			Body: "when a deploy hangs on rollback, drain the node before retrying",
		})
	if err != nil {
		t.Fatalf("create a page on member 0: %v", err)
	}

	// THE PREMISE IS ESTABLISHED ON THE WRITER, and only there. A write
	// this node has not applied yet is not an acknowledged one, and the
	// arm below is about what a PEER can see of a write that HAS been —
	// so the wait belongs here rather than around the reads.
	if err := c.nodes[0].engine.WaitCommitted(t.Context(), written.Position); err != nil {
		t.Fatalf("member 0 never applied its own create: %v", err)
	}

	// (1) NO WAIT LOOP ANYWHERE BELOW. A linearizable read is defined as
	// "no answer from before this read arrived", so a poll around THESE
	// reads would turn the assertion into one about timing, and the case
	// would pass on a build with no barrier at all.
	for i, n := range c.nodes {
		detail, err := n.engine.Tracker().Task(t.Context(), written.Key,
			tracker.DetailWants{}, statelog.Freshness{Level: statelog.ReadLinearizable})
		if err != nil {
			t.Fatalf("member %d refused a linearizable read of %s: %v — a read "+
				"level that cannot be served on a healthy fleet is a promise "+
				"the engine does not keep", i, written.Key, err)
		}
		if detail.Task.Title != "the linearizable one" {
			t.Errorf("member %d answered %q for %s", i, detail.Task.Title,
				written.Key)
		}
	}

	// (2) AND THE LEVEL SERVED IS THE LEVEL ASKED FOR. A read level never
	// silently downgrades — the two can only differ by a refusal — so a
	// member answering `session` to a linearizable question has answered a
	// different question and said nothing about it.
	for i, n := range c.nodes {
		answer, err := n.engine.Tracker().Tasks(t.Context(), tracker.Query{
			Scope: tracker.Scope{Workspace: true},
			Level: statelog.ReadLinearizable, Limit: 200,
		}, time.Now())
		if err != nil {
			t.Fatalf("member %d could not list the board: %v", i, err)
		}
		if answer.Level != statelog.ReadLinearizable {
			t.Errorf("member %d asked for a %s read and was served %s",
				i, statelog.ReadLinearizable, answer.Level)
		}
		if len(answer.Rows) == 0 {
			t.Fatalf("member %d's board is empty after a create", i)
		}
	}

	// (3) AND THE KNOWLEDGE BASE, which reached the log after the tracker
	// did and is the domain a half-finished adoption would strand.
	//
	// LINEARIZABLE, on the same terms as the tracker arm above: pages is a
	// registered domain with its own barrier subject and its own read
	// index, and a fleet that agreed about work while its wiki answered
	// from whatever each node happened to hold would be exactly the
	// half-adopted domain this arm exists to catch.
	for i, n := range c.nodes {
		if err := n.engine.WaitCommitted(t.Context(), page.Outcome.Position); err != nil {
			t.Fatalf("member %d never applied the page: %v", i, err)
		}
		got, err := n.engine.Pages().Get(t.Context(), page.Page.ID,
			statelog.Freshness{Level: statelog.ReadLinearizable})
		if err != nil {
			t.Fatalf("member %d cannot read a page member 0 wrote: %v", i, err)
		}
		if got.Page.Title != "Fleet rollback runbook" {
			t.Errorf("member %d holds %q for the page member 0 titled %q",
				i, got.Page.Title, "Fleet rollback runbook")
		}
	}

	// THE DIGEST TELLS DIFFERENT ROWS APART, checked before it is trusted
	// to say two members agree. It is the same shape as every other
	// control in this repository: a comparison that always matches
	// certifies a fleet whatever its rows are.
	if digestRows([]string{"a", "b"}) == digestRows([]string{"a", "c"}) {
		t.Fatal("two different row sets digest alike, so the comparison below " +
			"would report every member a twin of every other whatever they hold")
	}
	if digestRows([]string{"a", "b"}) != digestRows([]string{"b", "a"}) {
		t.Fatal("one row set in two orders digests differently — the digest is " +
			"a property of the SET, and an applier free to write two rows in " +
			"either order would fail this comparison on a healthy fleet")
	}

	// (4) AND THE MEMBERS ARE TWINS, ROW FOR ROW — LAST, because it can
	// only mean anything once every member has applied everything above.
	// The board listing was one query's answer; this is every REPLICATED
	// table of every registered domain, compared as a digest.
	//
	// The class is what makes the comparison meaningful rather than
	// merely strict: `Divergent` tables are written by an apply and still
	// legitimately differ (a node records what IT deferred), and `Local`
	// ones are this node's own — so comparing every table would fail on a
	// healthy fleet, and comparing only the board would pass on one whose
	// domains had quietly diverged underneath it.
	twins := map[string]string{}
	for i, n := range c.nodes {
		digest := replicatedDigest(t, n)
		if len(digest) == 0 {
			t.Fatalf("member %d reported no replicated tables — the comparison "+
				"below would hold between two empty maps", i)
		}
		if i == 0 {
			twins = digest
			continue
		}
		for table, want := range twins {
			if got := digest[table]; got != want {
				t.Errorf("member %d's %s digests %s and member 0's digests %s "+
					"— N identical copies is what the whole framework is for, "+
					"and two members disagreeing about one company looks from "+
					"either screen exactly like the other node being idle",
					i, table, got, want)
			}
		}
		for table := range digest {
			if _, both := twins[table]; !both {
				t.Errorf("member %d holds replicated table %s and member 0 "+
					"does not", i, table)
			}
		}
	}
}

// replicatedDigest is one member's REPLICATED rows, per table, as a digest.
//
// # Why a digest rather than the rows
//
// A failure has to name WHICH table disagrees, and a diff of two hundred rows
// names nothing a reader can act on. The digest is over the rows in a declared
// order, so it is the same on both members whenever the rows are — and the
// table name beside it is what sends somebody to the right applier.
//
// The domain list comes from the ENGINE rather than from a copy here, because
// a second list is how a fourth domain is silently left uncompared.
func replicatedDigest(t *testing.T, n *node) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, domain := range n.engine.Domains() {
		for table, class := range domain.Tables() {
			if class != statelog.Replicated {
				continue
			}
			out[table] = digestTable(t, n, table)
		}
	}
	return out
}

// digestTable hashes one table's rows in a stable order.
//
// EVERY COLUMN, read as text through the driver's own rendering and separated
// by a byte no column value can contain, so two different splits of one row
// cannot hash alike. The order is the table's own columns and a sort over the
// rendered row, which is what makes the digest a property of the SET of rows
// rather than of the order an applier happened to write them in.
func digestTable(t *testing.T, n *node, table string) string {
	t.Helper()
	var rendered []string
	if err := n.engine.Backends().Store.Replicated().Read(t.Context(),
		func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(t.Context(), `SELECT * FROM `+table)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			columns, err := rows.Columns()
			if err != nil {
				return err
			}
			for rows.Next() {
				cells := make([]any, len(columns))
				into := make([]any, len(columns))
				for i := range cells {
					into[i] = &cells[i]
				}
				if err := rows.Scan(into...); err != nil {
					return err
				}
				parts := make([]string, 0, len(cells))
				for _, cell := range cells {
					parts = append(parts, fmt.Sprintf("%v", cell))
				}
				rendered = append(rendered, strings.Join(parts, "\x1f"))
			}
			return rows.Err()
		}); err != nil {
		t.Fatalf("digest %s: %v", table, err)
	}
	return digestRows(rendered)
}

// digestRows hashes rendered rows in a stable order.
//
// SPLIT OUT SO IT CAN BE EXERCISED DIRECTLY. A digest that ignored its input
// would make every table on every member agree, and the comparison above would
// certify a fleet whatever its rows were — an absence assertion passes
// identically when the thing is absent and when the check has gone inert.
func digestRows(rendered []string) string {
	ordered := slices.Clone(rendered)
	slices.Sort(ordered)
	sum := sha256.Sum256([]byte(strings.Join(ordered, "\x1e")))
	return fmt.Sprintf("%x (%d rows)", sum[:8], len(ordered))
}

// A MEMBER THAT CANNOT BE REACHED FALLS SILENT WITHIN THE BUDGET, AND COMES
// BACK WHEN THE NETWORK DOES.
//
// # Why this needs a real partition
//
// [search]'s partial answer — `buckets_answered`, `buckets_missing` and the
// nodes it names in `Absent` — is arithmetic over which peers replied, and
// `internal/search`'s own cases pin that arithmetic against a fake peer that
// returns nothing. What they cannot show is the thing that makes a peer return
// nothing in production: a route that is down. A fake that answers instantly
// with an empty slice and a member that is unreachable are the same value and
// completely different failures, and only one of them can hang a search.
//
// So this is the network half, and the only case in this package that takes
// [startPartitionableCluster]. Three claims, in the one order that can prove
// them:
//
//  1. THE SCATTER RETURNS RATHER THAN WAITING OUT THE CALLER. A search whose
//     coordinator blocks on an absent member is worse than a partial answer —
//     it is a fleet where losing one node stops every search on every node,
//     which is precisely what holding the whole corpus on each member is meant
//     to prevent.
//  2. THE SILENCE IS THE PARTITION. The same scatter is run BEFORE the cut and
//     must come back full, or a missing answer afterwards would prove only that
//     nobody was ever listening.
//  3. AND ONLY THE PARTITION. The member the cut did not touch answers through
//     it, so what falls silent is the one member that is unreachable and not
//     the asker's whole view of the fleet — which from the answer alone would
//     read as the same partial result, with every peer named absent.
//
// The scatter rides core NATS request/reply — no stream, no consumer, no ack,
// per [queue]'s `Ask`/`Serve` — so cutting a member's routes is exactly what
// makes it unreachable. The other two keep their quorum across the cut, but
// quorum is not what this measures.
func TestAPartitionedMemberIsSilentRatherThanSlow(t *testing.T) {
	noParallel(t)
	c := startPartitionableCluster(t, fleetSize)
	c.hydrated(t)

	self := c.nodes[0].id
	var peers []search.Assigned
	for _, a := range search.Divide(c.nodeIDs()) {
		if a.Node != self {
			peers = append(peers, a)
		}
	}
	if len(peers) != fleetSize-1 {
		t.Fatalf("a fleet of %d left %d peers to ask", fleetSize, len(peers))
	}

	query := search.FanQuery{Text: "rollback drain node", Sources: []string{"page"}}
	scatter := func(budget time.Duration) ([]search.Slice, time.Duration) {
		t.Helper()
		started := time.Now()
		deadline, cancel := context.WithTimeout(t.Context(), budget)
		defer cancel()
		// AN ERROR AND AN EMPTY ANSWER ARE THE SAME FACT HERE — nothing
		// came back — and [search.FanOut] treats them identically: a
		// scatter that fails costs the answer its peers' buckets and is
		// reported as missing rather than as a failed search. So the
		// count is what this asserts, and the error is not.
		out, _ := search.Broker{Queue: c.nodes[0].engine.Backends().Queue}.
			Scatter(deadline, query, peers)
		return out, time.Since(started)
	}

	// EVERY PEER ANSWERS FIRST. Without this the silence below is
	// unfalsifiable: a responder that never registered looks the same.
	waitFor(t, "every peer to answer a scatter", func() bool {
		got, _ := scatter(clusterSettle)
		return len(got) == len(peers)
	})

	c.relays.Partition(t, 1)
	cutOff := c.nodes[1].id

	// A BUDGET WELL UNDER clusterSettle, because what is being measured is
	// that the wait ENDS at the caller's deadline rather than at the
	// broker's own timeout — a scatter that returned only after the
	// default would pass a generous assertion while still holding a real
	// search open for far longer than its caller allowed.
	const cut = 2 * time.Second
	got, took := scatter(cut)
	answered := map[string]bool{}
	for _, slice := range got {
		answered[slice.Node] = true
	}
	if answered[cutOff] {
		t.Errorf("the partitioned member %s answered — the cut route did not "+
			"stop the request, so this case measures nothing", cutOff)
	}
	for _, peer := range peers {
		if peer.Node != cutOff && !answered[peer.Node] {
			t.Errorf("%s, which the cut did not touch, did not answer either "+
				"(answers from %v) — the partition silenced more than the "+
				"member it cut", peer.Node, slices.Sorted(maps.Keys(answered)))
		}
	}
	if took > cut+cut/2 {
		t.Errorf("the scatter took %s against a %s budget — a coordinator that "+
			"waits out an absent member turns one lost node into every node's "+
			"search stalling", took, cut)
	}

	// AND THE DEGRADATION IS NOT ONE-WAY. A fleet that never readmitted a
	// member after a blip would answer partially for ever, and the partial
	// report would be a permanent state rather than a passing one.
	c.relays.Heal(t, 1)
	waitFor(t, "the healed member to answer again", func() bool {
		back, _ := scatter(clusterSettle)
		return len(back) == len(peers)
	})
}
