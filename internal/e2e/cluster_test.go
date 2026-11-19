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

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A FLEET, end to end: n engines, each embedding its own broker, clustered.
//
// A second constructor, not a replacement for [start], the shape a quickstart
// runs (one node, no quorum): making it a degenerate cluster would put a route
// mesh, a metadata group and a replica count under sixty tests whose subject is
// none of them. A fleet adds replication, quorum, leader election, placement,
// seat handover, and a node too far behind adopting a peer's snapshot.
//
// Every route runs through a per-ordered-pair relay this process owns, so a
// case can cut one member off and restore it. That costs n(n-1) listeners and a
// copy of every route byte, and is the only way to exercise a partition:
// stopping a listener leaves NATS's established routes working indefinitely.
type cluster struct {
	nodes  []*node
	relays *jetstreamtest.Relays
}

// startCluster stands up n nodes, each an engine and its API, on one clustered
// broker. tune amends every member's bootstrap after the harness's own
// settings and before anything is seeded or opened from it
// ([memberBootstrap]) — [TestAFleetOfThree] takes one member's seats away
// with it ([withoutSeats]).
func startCluster(t *testing.T, n int, tune ...func(*config.Bootstrap)) *cluster {
	t.Helper()
	if n < 2 {
		t.Fatalf("startCluster(%d): use start(t) for one node", n)
	}
	return startMesh(t, jetstreamtest.StartDirectMesh, n, tune...)
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
func startMesh(t *testing.T, mesh func(context.Context, *testing.T, int) *jetstreamtest.Relays,
	n int, tune ...func(*config.Bootstrap)) *cluster {

	t.Helper()
	logs.attribute(t)
	// BOUNDED IN WALL CLOCK AS WELL AS IN TRIES, see
	// [jetstreamtest.ClusterStartBudget]: attempts at an unbounded cost each
	// were measured at 1077 seconds across three cases, 64% of the run.
	//
	// AND THE DEADLINE IS HANDED TO THE ATTEMPT, not only consulted after
	// it: checked between attempts it bounds how MANY, not how long one
	// takes, and buildMember reaches engine.New on the test's own context,
	// whose bring-up may spend [jsprovision.SequenceBudget], so one attempt
	// could outlast the ceiling. A budget the work cannot observe is a
	// comment rather than a bound.
	deadline := time.Now().Add(jetstreamtest.ClusterStartBudget)
	// THE HOOK'S ARGV IS THE SAME FOR EVERY MEMBER OF EVERY ATTEMPT, and
	// finding this test binary can fail the test — which only this
	// goroutine may do, never a member's ([startMeshOnce]). So is the git
	// Gitlet runs on every member, held here to the node floor: the
	// fleet's company hosts code ([fleetCompanyDoc]).
	hook := gitlettest.HookCommand(t)
	gittest.RequireNode(t)
	for attempt := 1; attempt <= clusterStartAttempts; attempt++ {
		// THE MESH IS BUILT INSIDE THE ATTEMPT: reserving ports and starting
		// relays is part of what the ceiling bounds, and
		// [jetstreamtest.StartRelays] has no budget of its own that could
		// leave the member start an expired context.
		// THE SOONER OF THE CEILING AND THIS ATTEMPT'S OWN TERM, from
		// [jetstreamtest.StartAttemptEnd]: handed the whole remaining
		// budget, attempt one could spend all of it (181s of a 180s ceiling,
		// "1 of 4 attempts" in the failure).
		attemptCtx, cancelAttempt := context.WithDeadline(t.Context(),
			jetstreamtest.StartAttemptEnd(time.Now(), deadline))
		relays := mesh(attemptCtx, t, n)
		c, err := startMeshOnce(attemptCtx, t, relays, n, hook, tune...)
		cancelAttempt()
		if err == nil {
			return c
		}
		// THE FAILED ATTEMPT'S MESH GOES DOWN NOW, beside the members
		// [startMeshOnce] already stopped: its relay listeners hold the
		// ports, and the next attempt is about to ask for a fresh set.
		relays.Stop()

		// RETRY BY DEFAULT, AND NAME WHAT ACTUALLY FAILED. Standing up n
		// brokers on a shared machine fails for timing far more often than
		// anything else (a member that could not form its cluster in the
		// budget, a lost port, a metadata group still electing), which is
		// what the attempts absorb. Only a failure KNOWN to repeat
		// identically ends the test: guessing wrong costs a case that
		// would have passed (gating on a lost port alone ended three cases
		// on an `ensure stream ...: context deadline exceeded` a second
		// attempt always absorbed). Nor may the log call every failure a
		// port race, naming a cause the run had no evidence for.
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
// a ceiling the member starts cannot observe bounds nothing. hook is every
// member's proc-receive hook ([buildMember]).
func startMeshOnce(ctx context.Context, t *testing.T, relays *jetstreamtest.Relays, n int,
	hook []string, tune ...func(*config.Bootstrap)) (*cluster, error) {

	t.Helper()
	c := &cluster{relays: relays, nodes: make([]*node, n)}
	// ONE KEYRING FOR THE MESH, minted once and handed to every member.
	// Every record on every state log is signed under it, so members that
	// did not share one would refuse each other's records — which is a
	// property these cases exist to exercise rather than to trip over.
	ringID, ringMaterial := fleetKeyring(t)

	// EVERY MEMBER'S BOOTSTRAP IS ASSEMBLED, AND ITS STORE SEEDED, HERE, on
	// the test's own goroutine before any member starts: a seed that cannot
	// be written fails the test, which a member's goroutine may not do (see
	// below), and none of it binds anything, so nothing about it has to wait
	// for the moment a member starts.
	boots := make([]config.Bootstrap, n)
	for i := range n {
		boot, err := memberBootstrap(t, relays, i, n, ringID, ringMaterial, tune...)
		if err != nil {
			return nil, fmt.Errorf("member %d: %w", i, err)
		}
		seedStore(t, &boot)
		boots[i] = boot
	}

	// CONCURRENTLY, which is what a fleet actually does: n machines boot
	// independently. Sequentially is not merely slower — it does not work.
	// A clustered member does not finish starting until its JetStream has
	// caught up with the metadata group, and that group needs a majority,
	// so the first member would wait out its whole readiness budget for
	// peers this loop has not started yet and then fail naming a cluster
	// nobody could have formed.
	errs := make([]error, n)
	stops := make([]teardown, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// t.Fatalf is the test goroutine's alone, so a member's
			// failure is carried back rather than raised here — a
			// FailNow from another goroutine ends that goroutine and
			// leaves the test running with a nil member.
			c.nodes[i], stops[i], errs[i] = buildMember(ctx, t, &boots[i], i, hook)
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

	// EVERY MEMBER THIS ATTEMPT STARTED IS STOPPED BEFORE THE NEXT ONE:
	// the difference between a retry and a pile-up.
	//
	// A member left running still applies, heartbeats and competes for the
	// replacement's cores, and its broker still gossips under the cluster
	// name the next attempt's members use, so any that comes up forms a
	// cluster with the ghost: attempt 1 timing out on one member, attempt 2
	// never ready, attempt 3 worse.
	//
	// t.Cleanup cannot do this: it runs when the TEST ends, after every
	// attempt.
	stopAll(stops)
	// THE ERROR GOES BACK rather than being judged here: whether it is
	// worth another attempt is the caller's question, and it is the only
	// frame that knows how many are left.
	return nil, fmt.Errorf("member %d: %w", failed, errs[failed])
}

// teardown is one member's undo, in the three phases [stopAll] runs a fleet's
// in: the API in front of the engine, the engine with the chat hub beside it
// ([newEngine]), and what the engine runs on — its broker and store
// ([engine.Backends]) and its model server. Each phase's pieces are in the
// order they came up.
type teardown struct {
	api, engine, backends []func()
}

// stopAll takes a fleet down, one phase at a time and each phase on every
// member AT ONCE: every API, then every engine, then every broker and store.
//
// THE BROKERS GO LAST, AFTER EVERY ENGINE HAS STOPPED, which is why a member's
// backends are opened by the harness and lent to its engine
// ([engine.Options.Backends]): an engine's stop takes coordination steps that
// need a quorum of the fleet's brokers. Stopped one after another, the last
// spent its whole allowance on steps that could not succeed (10 to 25 s);
// stopped together but each closing its own broker, a slower peer was
// stranded the same way in about half the runs. EVERY API BEFORE ANY ENGINE.
// Within a member each phase runs in REVERSE of build order: the projector
// reads the queue the engine owns, and the server serves the app.
func stopAll(members []teardown) {
	phase := func(of func(teardown) []func()) {
		var wg sync.WaitGroup
		for _, m := range members {
			wg.Add(1)
			go func() {
				defer wg.Done()
				stopInReverse(of(m))
			}()
		}
		wg.Wait()
	}
	phase(func(m teardown) []func() { return m.api })
	phase(func(m teardown) []func() { return m.engine })
	phase(func(m teardown) []func() { return m.backends })
}

// clusterStartAttempts is how many times a fleet is stood up before the case
// gives up.
//
// [jetstreamtest.ClusterStartAttempts] ITSELF, not a second spelling of its
// argument: restating it once drifted (three against four). The collision is
// with work this process does not coordinate with, so a wider window does not
// help and noticing does, and each attempt reserves its own mesh, which is
// what makes "trying again" mean something. See [startMesh].
const clusterStartAttempts = jetstreamtest.ClusterStartAttempts

// clusterHost is the interface every member of a test mesh binds its route
// listener on. LOOPBACK, because the mesh's own route URLs are loopback and a
// member listening wider would be reachable from outside the harness — and
// because the pre-bind probe asks about one address, which cannot answer for a
// wildcard bind.
const clusterHost = "127.0.0.1"

// memberBootstrap is member i's half of Tier A: where it keeps its data, the
// mesh it routes through, and the fleet-wide settings every member shares —
// the keyring among them, which is the mesh's one ring — and, after all of
// those, whatever the case's tune amends.
//
// EVERY NODE HAS ITS OWN STORE AND ITS OWN STREAM DIRECTORY, because that is
// what a fleet is: n machines, each with its own disk. Sharing either would
// make this one node wearing three hats, and every fleet mechanism under it
// would pass for the wrong reason.
func memberBootstrap(t *testing.T, relays *jetstreamtest.Relays, i, n int,
	ringID, ringMaterial string, tune ...func(*config.Bootstrap)) (config.Bootstrap, error) {

	port, routes, advertise := relays.Member(i)
	peers, err := otherMembers(routes, port)
	if err != nil {
		return config.Bootstrap{}, fmt.Errorf("%w: member %d's routes: %w",
			errNotRetryable, i, err)
	}
	boot := config.DefaultBootstrap()
	boot.Node.ID = fmt.Sprintf("node-%d", i)
	// ONE RING FOR THE WHOLE MESH. A member keyed differently would
	// refuse every peer's records, which is exactly what these cases
	// exist to prove cannot happen by accident.
	boot.Secrets.ActiveKeyID, boot.Secrets.Keys = ringID, []config.SecretKey{
		{ID: ringID, Material: ringMaterial},
	}
	withCredential(t, &boot)
	// ONE ADDRESS AND ONE SIGN-IN FOR THE WHOLE FLEET, as behind a load
	// balancer: a browser signs in on whichever member it reaches and
	// presents that cookie to every other. See [withSignIn].
	withSignIn(&boot)
	boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&boot.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	brokertest.GitletCacheIn(&boot.Gitlet)
	boot.Stream.Cluster.Name = relays.ClusterName()
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
	// ONE LEASE STORE FOR THE FLEET, which makes it a fleet rather than n
	// nodes sharing a log. The default, `local`, keeps every lease in this
	// process (Tier A refuses it on a clustered stream), so each member
	// claimed every seat and duty itself, every fleet singleton ran once
	// per member, and every membership read counted one node. It went
	// unnoticed because the loader validates a FILE and this bootstrap is
	// built in code; engine.New validates it now, which is also why
	// [fleetSize] is three.
	boot.Coordination.Type = config.CoordinationEmbeddedKV
	// AND A PERSON'S TOKEN: a git push is a person's own, under a token the
	// identity directory binds to their human seat ([bindDev] binds this
	// one to the developer), and one bound to nobody fetches only.
	withDevCredential(&boot)
	// LAST, so a case can amend anything above it — and before anything
	// reads it: the store is seeded from it ([startMeshOnce]) and the
	// broker and the engine are opened from it ([buildMember]), which is
	// the only moment Tier A is read at all.
	for _, amend := range tune {
		amend(&boot)
	}
	return boot, nil
}

// buildMember brings up member i of the fleet on the bootstrap
// [memberBootstrap] assembled for it, its code cache execing hook as its
// proc-receive hook.
//
// ctx is the ATTEMPT's, so the retry loop's wall-clock ceiling can interrupt a
// bring-up rather than only refuse the next one — see [startMesh].
//
// It runs on a member's own goroutine ([startMeshOnce]), so nothing in it may
// fail the test: whatever could is done before, on the test's goroutine, and
// handed in.
func buildMember(ctx context.Context, t *testing.T, boot *config.Bootstrap, i int, hook []string) (
	*node, teardown, error) {

	// THE TEARDOWN IS RETURNED, NOT REGISTERED WITH t.Cleanup, because an
	// attempt that fails has to stop what it started BEFORE the next one
	// starts — see [startMeshOnce]. It is built up as each piece comes up,
	// so a member that fails halfway still hands back a way to undo the
	// half that worked.
	var stops teardown
	fail := func(err error) (*node, teardown, error) { return nil, stops, err }

	// ITS HARNESS KEEPS ITS STATE ON THE MEMBER'S OWN DISK, beside its store, as
	// each machine of a fleet keeps its own: a directory [memberBootstrap]
	// made on the test's goroutine, since this one may not fail the test.
	model := newScriptedModelIn(t, filepath.Join(filepath.Dir(boot.Store.Path), "llm-harness"))
	// THE MODEL SERVER IS THIS ATTEMPT'S, not the test's. Its own
	// constructor registers a t.Cleanup as well, which is right for the
	// single-node cases — but a cluster case that retries three times would
	// otherwise leave one live server per member per failed attempt running
	// until the case ends. Close is idempotent, so both fire safely.
	stops.backends = append(stops.backends, model.close)
	// THE FLEET'S COMPANY HOSTS CODE, for its code claim. A single-node case
	// keeps companyDoc unless it opts in with [withCode], as the restore
	// case does: Gitlet is a git pool and a cache on every node, which
	// a case that pushes nothing would pay for nothing.
	cfg, err := config.ParseCompany([]byte(fleetCompanyDoc(model)))
	if err != nil {
		// THE SAME FILE PARSES THE SAME WAY on every attempt.
		return fail(fmt.Errorf("%w: company config: %w", errNotRetryable, err))
	}

	// TIER A'S OWN RULES, held — and the bootstrap normalized — before the
	// broker is opened from it, as engine.New does before it opens its own:
	// the backends below are opened HERE, so this is the first frame that
	// reads the bootstrap. A refusal is the same on every attempt.
	if err := boot.Validate(); err != nil {
		return fail(fmt.Errorf("%w: bootstrap: %w", errNotRetryable, err))
	}

	// PROBED IMMEDIATELY BEFORE THE BROKER BINDS IT, the guard
	// [jetstreamtest.Cluster.start] has and this path did not.
	//
	// It cannot close the race, but it shrinks the window to microseconds
	// and names the CAUSE. Its partner is the broker's own refusal of a
	// bind lost inside that window (a nats-server Fatalf, reported at once
	// as [jetstream.ErrRoutePortTaken]); together they turn a lost port
	// into an immediate, named retry.
	//
	// THE SAME HOST THE MEMBER BINDS, set by [memberBootstrap]: a probe
	// against a different address answers about a port the server never
	// asks for.
	port := boot.Stream.Cluster.Port
	switch free, err := jetstreamtest.PortFree(ctx, boot.Stream.Cluster.Host, port); {
	case err != nil:
		// NOT A RACE: an address this host does not have, or a probe
		// that never ran. Retrying it would spend every attempt on a
		// mistake that answers the same way each time.
		return fail(fmt.Errorf("%w: route port %d on %q cannot be probed for "+
			"member %d: %w", errNotRetryable, port, boot.Stream.Cluster.Host, i, err))
	case !free:
		return fail(fmt.Errorf("%w — route port %d went between this mesh "+
			"reserving it and member %d starting",
			jetstream.ErrRoutePortTaken, port, i))
	}

	// THE BROKER AND STORE ARE THE HARNESS'S, LENT TO THE ENGINE
	// ([engine.Options.Backends]), so they outlive it: a fleet's brokers go
	// only once every member's engine has stopped — see [stopAll]. Opened by
	// the call engine.New makes for an engine that owns them, so the member
	// runs on what `crewlet run` would have opened.
	backends, err := engine.OpenBackends(ctx, boot, cfg, nil)
	if err != nil {
		return fail(refusal("open backends", err))
	}
	// ON WithoutCancel, like every teardown here: the attempt's context is
	// cancelled the moment the attempt ends, and a stop that inherited it
	// would be handed a dead context exactly when it has work to do — the
	// rule internal/engine states for a rollback, applied to a harness.
	stops.backends = append(stops.backends,
		func() { backends.Close(context.WithoutCancel(ctx)) })

	// THE MEMBER'S BRIDGE LISTENER, bound before its engine names it — see
	// [listenForBridge] — and closed with the attempt if no server took it.
	ln, err := listenForBridge("")
	if err != nil {
		return fail(err)
	}
	stops.api = append(stops.api, func() { _ = ln.Close() })
	// A RECORDER OF ITS OWN, as `crewlet run` hands every engine one: what
	// only the member that did something measures — which member committed
	// a push — is read off it ([node.metrics]).
	recorder, err := metrics.New()
	if err != nil {
		// THE SAME CATALOGUE VALIDATES THE SAME WAY on every attempt.
		return fail(fmt.Errorf("%w: metrics recorder: %w", errNotRetryable, err))
	}
	// THE ENGINE AND ITS CHAT HUB, as on every node ([newEngine]) — the hub
	// stopped in the engine's own phase and before it, since the hub answers
	// on the broker the engine stops with.
	e, chatLive, engineStops, err := newEngine(ctx, engine.Options{
		Bootstrap: boot, Company: cfg, ActivatedAt: harnessActivation,
		Environment: nodeEnvironment(nil), Backends: backends, Metrics: recorder,
		SandboxPollInterval: runPollInterval, BoxHostURL: listenerURL(ln),
		// The proc-receive hook every member's cache execs: this test
		// binary, for [startNode]'s reason — computed once, on the test's
		// own goroutine ([startMesh]), never here.
		GitHook: hook,
	})
	stops.engine = append(stops.engine, engineStops...)
	if err != nil {
		return fail(refusal("engine.New", err))
	}
	// ON THE TEST'S CONTEXT, NOT THE ATTEMPT'S: a context that ends while an
	// engine starts is that engine's stop, which begins its drain
	// ([engine.Engine.Start]), so an attempt whose deadline passed mid-start
	// would hand back a member that started without error and never claims a
	// seat — reported as a cluster that came up, and failing later on
	// whatever the case reads first. The test's context is the member's
	// lifetime, as it is wireAPI's below.
	if err := e.Start(t.Context()); err != nil { //nolint:contextcheck // see above
		return fail(fmt.Errorf("engine.Start: %w", err))
	}

	// THROUGH wireAPI, NOT serveAPI: this runs off the test's goroutine,
	// where t.Fatalf would end only this goroutine, and the API's teardown
	// belongs in this member's own, in the phase [stopAll] runs before the
	// engines', so a failed attempt stops it before the engine it reads from.
	//
	// THE SUPPRESSION BELOW: the context handed over is deliberately NOT
	// this attempt's. `ctx` is cancelled when the bring-up ends, and what
	// wireAPI starts under it serves the whole case. The test's context is
	// that lifetime exactly; [context.WithoutCancel] would outlive the case.
	// AT THE PRODUCTION TICK: see [tickInterval].
	app, srv, apiStops, err := wireAPI(t.Context(), e, boot, chatLive.Hub(), ln, //nolint:contextcheck // see above
		func(opts *api.Options) { opts.HealthInterval = stream.HealthInterval })
	stops.api = append(stops.api, apiStops...)
	if err != nil {
		return fail(fmt.Errorf("api: %w", err))
	}

	return &node{
		engine: e, app: app, server: srv, model: model,
		id:          e.Node().ID(),
		snapshotDir: boot.Store.SnapshotDirFor(),
		chat:        chatLive,
		metrics:     recorder,
	}, stops, nil
}

// refusal names what failed to come up, and marks a refusal that repeats as
// not worth another attempt: it is refused identically on every one, so a
// fresh mesh would spend the whole start budget re-asking a question already
// answered, and then report it as a cluster that never came up.
//
// Two refusals repeat. A refused CONFIG — the same file parses the same way.
// And a broker that declared itself dead ([jetstream.ErrBrokerDied]): what
// nats-server calls fatal is a fact about the configuration or the host, a
// store directory it cannot use or an address it cannot read — except a port
// somebody else took between this mesh reserving it and the member binding
// it, which the broker reports as [jetstream.ErrRoutePortTaken] or
// [jetstream.ErrLeafPortTaken] and is the race the attempts are for.
func refusal(what string, err error) error {
	var fault *config.Fault
	died := errors.Is(err, jetstream.ErrBrokerDied) &&
		!errors.Is(err, jetstream.ErrRoutePortTaken) &&
		!errors.Is(err, jetstream.ErrLeafPortTaken)
	if errors.As(err, &fault) || died {
		return fmt.Errorf("%w: %s: %w", errNotRetryable, what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
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
// the omission is deliberate.
//
// A three-member cluster is three embedded NATS servers, three pairs of SQLite
// databases and over twenty raft groups on one machine. Two at once compete for
// one disk and scheduler and raft gives way first: each case passes alone in
// under forty seconds and both fail together, as a boot failure naming a stream.
//
// NOR BESIDE THE SINGLE-ENGINE CASES: `go test` starts a parallel top-level
// test only once every sequential one has returned, so every fleet runs and
// comes down first, against a quiet runner. A function so a later edit does
// not answer "why is this one not parallel" by adding it back.
func noParallel(t *testing.T) {
	t.Helper()
	t.Setenv("CREWLET_CLUSTER_CASE", "1") // Setenv also forbids t.Parallel.
}

// fleetSize is how many members a cluster case stands up.
//
// THREE, the smallest fleet Tier A will run: two embedded members on the
// embedded KV are refused (`stream.cluster.peers`, in
// [config.Bootstrap.Validate]) because two have no coordination quorum without
// each other, so a rolling upgrade, restarting them one at a time, stops the
// fleet. engine.New holds every bootstrap to it.
//
// Cost no longer argues for two: this solo package ([solo.Run]) runs at -p 1
// with no two cluster cases at once ([noParallel]), and measured under -race
// three members cost 494 s against 476 s at two (about 4%). The third buys a
// MAJORITY: the partition case cuts one off while two keep quorum.
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

// whole is the condition a member of a healthy fleet meets: its native
// backends have hydrated, and it counts every member live. HYDRATED ALONE
// does not say a fleet is whole — an engine that stopped keeps the answer it
// last had — so the member's own view of the fleet's presence is read too,
// which a member that stopped or lost its peers changes at once.
//
// Each presence read is bounded by [engine.ProbeReadBudget], the bound the
// health envelope gives the same read: it is a scan no client timeout covers,
// and an unbounded one against a store that stopped answering would hold the
// wait past its own budget.
func (c *cluster) whole(t *testing.T, n *node) func() bool {
	t.Helper()
	return func() bool {
		if !n.engine.StateLogHydrated(t.Context()) {
			return false
		}
		ctx, cancel := context.WithTimeout(t.Context(), engine.ProbeReadBudget)
		defer cancel()
		live, err := n.engine.LiveNodes(ctx)
		return err == nil && live == len(c.nodes)
	}
}

// fleetClaims are the claims [TestAFleetOfThree] makes of one fleet, in the
// order it makes them, each a subtest named for its claim.
//
// THE ORDER IS PART OF THE CLAIMS, which is what sharing a fleet costs:
//   - snapshots FIRST: it holds a snapshot taken AT BOOT, and later an
//     artefact could be one any claim caused;
//   - agreement LAST: it compares every replicated row of every member
//     ([cluster.digestsAtOneEnd]), so every row each earlier claim wrote;
//   - code just before it: it WRITES THE COMPANY'S CONFIGURATION (PUT /config),
//     so every claim it could disturb has run, and the last runs no turn;
//   - the rest read only what they wrote, under their own ids and paths, so
//     another claim's rows only add to what they read.
var fleetClaims = []struct {
	name string
	run  func(*testing.T, *cluster)
}{
	{"AFleetTakesAndOffersSnapshots", claimAFleetTakesAndOffersSnapshots},
	{"ARecordOneNodeWritesReachesEveryNodesRows", claimARecordOneNodeWritesReachesEveryNodesRows},
	{"EveryNodeMintsIntoOneKeySpace", claimEveryNodeMintsIntoOneKeySpace},
	{"TheFleetAnswersOneSearchBetweenItsMembers", claimTheFleetAnswersOneSearchBetweenItsMembers},
	{"AFileUploadedToOneNodeDownloadsFromAnother", claimAFileUploadedToOneNodeDownloadsFromAnother},
	{"TheCollectorsReportReadsOnEveryNode", claimTheCollectorsReportReadsOnEveryNode},
	{"AMessageOnOneNodeWakesASeatOnAnotherAndTheFleetAgrees",
		claimAMessageOnOneNodeWakesASeatOnAnotherAndTheFleetAgrees},
	{"PersonsPushAndFetchAcrossTheFleet", claimPersonsPushAndFetchAcrossTheFleet},
	{"AFleetAgreesAboutOneCompany", claimAFleetAgreesAboutOneCompany},
}

// A FLEET OF THREE, AND WHAT ONLY A FLEET CAN SHOW — on ONE fleet, since the
// claims are about a running fleet, not its boot. The claims are ordered
// ([fleetClaims]); a fleet that cannot be stood up fails every claim at once
// ([startMesh] retries on fresh ports); each claim starts on a hydrated fleet
// counting every member live ([cluster.whole]), else the case stops naming it.
// ONE MEMBER RUNS NO SEATS ([chatWriterNode], [withoutSeats]): the chat claim
// posts on a member the seat it wakes can never be on, which only a member
// without [placement.RoleSeats] makes a fact rather than a timing.
//
// THE PARTITION CLAIM KEEPS A FLEET OF ITS OWN
// ([TestAPartitionedMemberIsSilentRatherThanSlow]): its relays are costly
// ([jetstreamtest.StartDirectMesh]) and it breaks its fleet on purpose.
func TestAFleetOfThree(t *testing.T) {
	noParallel(t)
	c := startCluster(t, fleetSize, withoutSeats(chatWriterNode))
	for i, claim := range fleetClaims {
		after := "the fleet came up"
		if i > 0 {
			after = "the " + fleetClaims[i-1].name + " claim"
		}
		var rest []string
		for _, later := range fleetClaims[i:] {
			rest = append(rest, later.name)
		}
		for m, n := range c.nodes {
			waitFor(t, fmt.Sprintf("member %d to be hydrated and see all %d members "+
				"live after %s — %v are not run on a fleet that is not",
				m, len(c.nodes), after, rest), c.whole(t, n))
		}
		t.Run(claim.name, func(t *testing.T) { claim.run(t, c) })
	}
}

// withoutSeats is the tune that takes [placement.RoleSeats] from the member
// called id and leaves it every other role in the vocabulary.
//
// EVERY OTHER ROLE, AND STATED AS THE VOCABULARY LESS ONE rather than as a
// list: [placement.RoleData] is what every cluster member must hold and what
// ingress and workers both need (Tier A refuses the member otherwise, by
// name), and a role added to the vocabulary later is one this member should
// take as every other member does — a list written here would quietly leave
// it out. What is removed is the ability to RUN a seat, not to reach the
// fleet: the member still accepts writes and publishes them.
func withoutSeats(id string) func(*config.Bootstrap) {
	return func(b *config.Bootstrap) {
		if b.Node.ID != id {
			return
		}
		b.Node.Roles = nil
		for _, role := range placement.Vocabulary() {
			if role != placement.RoleSeats {
				b.Node.Roles = append(b.Node.Roles, string(role))
			}
		}
	}
}

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
func claimARecordOneNodeWritesReachesEveryNodesRows(t *testing.T, c *cluster) {
	// THE CHART FIRST. A create takes its key from its project's own
	// counter, so the project has to exist — every data node applies the chart
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
				worklet.DetailWants{}, statelog.Freshness{Level: statelog.ReadSession})
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
// A key is what people paste into chat, so a shared one opens the wrong record
// from somebody's link. The counter is arbitrated on the PROJECT's own subject:
// three nodes minting at once contend at the broker, one wins each round, and
// the losers read the winner's value and take the next. On one node that
// arbitration is untested; here all three file into one project at once, the
// shape that produced two ENG-1s in the design this replaced.
func claimEveryNodeMintsIntoOneKeySpace(t *testing.T, c *cluster) {
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
			var got worklet.WriteResult
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

// A NODE TAKES SNAPSHOTS AND SERVES THEM, the half of the recovery path a
// recipient cannot supply for itself.
//
// [statelog] certifies the snapshot loop and transfer against fakes, not that a
// running ENGINE arms them. Half-wired is the worst state: a node below the
// log's floor asks the fleet, nothing answers, and it stays on the history it
// has with one log line. So the assertion is on the artefact: a member has one
// on disk, its manifest names every registered domain, and a peer asking the
// fleet is offered it. Forcing a node below the floor needs a trim and so a
// backup, which is [internal/backup]'s suite.
func claimAFleetTakesAndOffersSnapshots(t *testing.T, c *cluster) {
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
	//
	// ASKED OF THE ENGINE rather than listed here: a literal list of three
	// outlived the org chart and the identity directory joining the
	// register, and certified neither. Every node runs every registered
	// domain, so the register is exactly what the artefact must name.
	var runs []string
	for _, domain := range engine.Domains() {
		runs = append(runs, domain.Name())
	}
	for _, want := range runs {
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
// [internal/search]'s cases prove the merge, coverage arithmetic and wire format
// over fixtures. Only here are REAL engines (own broker, store, index) answering
// each other's slice requests, the arm a single-node suite passes vacuously.
//
// The assertion is on COVERAGE, not only hits: every member holds the whole
// corpus, so a fan-out that fell back to the local scan returns the same
// documents; the buckets each answer came from tell them apart.
//
// The table is explicit because a search below [search.FanOutFloor] is answered
// by the asking node alone, and crossing the floor would measure the applier.
func claimTheFleetAnswersOneSearchBetweenItsMembers(t *testing.T, c *cluster) {
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
		page, err := writer.Create(t.Context(), pageOperator(), doclet.NewPage{
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
		if err := n.engine.WaitReached(t.Context(), last); err != nil {
			t.Fatalf("member %d never applied the corpus: %v", i, err)
		}
		searcher := n.engine.DocletSearcher()
		if searcher == nil {
			t.Fatalf("member %d runs no Doclet searcher", i)
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
				Methods: []search.Method{search.MethodLexical},
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
// Three things only a fleet can show, on the same writes. The LAST claim
// [TestAFleetOfThree] makes, so the twins arm compares every earlier row too.
//
//  1. A linearizable read on another member sees an acknowledged write with no
//     wait loop (the barrier append; [claimARecordOneNodeWritesReachesEveryNodesRows]
//     polls past it).
//  2. The members are twins: N identical SQL copies per domain, since two
//     answering differently looks from a screen like the other node being idle.
//  3. The knowledge base is one of those domains: pages reached the log after
//     the tracker, and this arm catches a half-adopted domain.
func claimAFleetAgreesAboutOneCompany(t *testing.T, c *cluster) {
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
		doclet.NewPage{
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
	if err := c.nodes[0].engine.WaitReached(t.Context(), written.Position); err != nil {
		t.Fatalf("member 0 never applied its own create: %v", err)
	}

	// (1) NO WAIT LOOP ANYWHERE BELOW. A linearizable read is defined as
	// "no answer from before this read arrived", so a poll around THESE
	// reads would turn the assertion into one about timing, and the case
	// would pass on a build with no barrier at all.
	for i, n := range c.nodes {
		detail, err := n.engine.Tracker().Task(t.Context(), written.Key,
			worklet.DetailWants{}, statelog.Freshness{Level: statelog.ReadLinearizable})
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
		answer, err := n.engine.Tracker().Tasks(t.Context(), worklet.Query{
			Scope: worklet.Scope{Workspace: true},
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
		if err := n.engine.WaitReached(t.Context(), page.Outcome.Position); err != nil {
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
	// only mean something once every member has applied everything above,
	// on EVERY domain's log ([cluster.digestsAtOneEnd]), compared as a
	// digest of every REPLICATED table.
	//
	// The class makes it meaningful: `Divergent` tables are written by an
	// apply and legitimately differ (a node records what IT deferred), and
	// `Local` ones are this node's own, so comparing every table would fail
	// a healthy fleet, and only the board would pass one whose domains had
	// quietly diverged.
	digests := c.digestsAtOneEnd(t)
	twins := map[string]string{}
	for i, digest := range digests {
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

// digestsAtOneEnd is every member's [replicatedDigest], taken once every member
// has applied every registered domain's log to the same end, and again if any
// log grew while they were read.
//
// It covers every domain the engine registers ([engine.Engine.Domains], as
// [replicatedDigest] reads it), not the ones a case wrote to: a member that had
// not yet applied a record on an unwritten log (a usage day, an embedding)
// would differ from its peers by timing, not divergence. The ends are read
// before and after, and the digests count only when nothing was appended in
// between, so the comparison is a property of the rows.
func (c *cluster) digestsAtOneEnd(t *testing.T) []map[string]string {
	t.Helper()
	var digests []map[string]string
	waitFor(t, "every member to hold every log to one end", func() bool {
		ends := c.logEnds(t)
		for i, n := range c.nodes {
			for _, end := range ends {
				ctx, cancel := context.WithTimeout(t.Context(), waitBudget)
				err := n.engine.WaitReached(ctx, end)
				cancel()
				if err != nil {
					t.Fatalf("member %d never applied %s to its end %s: %v",
						i, end.Stream, end, err)
				}
			}
		}
		digests = digests[:0]
		for _, n := range c.nodes {
			digests = append(digests, replicatedDigest(t, n))
		}
		return slices.Equal(ends, c.logEnds(t))
	})
	return digests
}

// logEnds is every registered domain's log end: its last sequence as the
// stream's leader answers it, at the generation member 0's applier stands
// at — the position a member has applied the whole log at.
func (c *cluster) logEnds(t *testing.T) []statelog.Position {
	t.Helper()
	n := c.nodes[0]
	js, err := n.engine.Backends().API().Client(n.engine.Backends().Conn())
	if err != nil {
		t.Fatalf("a JetStream client on member 0: %v", err)
	}
	var out []statelog.Position
	for _, domain := range engine.Domains() {
		name := domain.Stream().Name
		// THE LEADER'S END, looked up as the product looks a stream up
		// ([jsprovision.LookupStream]): re-asked while nobody answers, and
		// never a member's own copy, which can be short of the quorum's.
		info, err := jsprovision.LookupStream(t.Context(), js, name)
		if err != nil {
			t.Fatalf("read %s's end: %v", name, err)
		}
		generation, err := n.engine.StreamGeneration(name)
		if err != nil {
			t.Fatalf("read %s's generation on member 0: %v", name, err)
		}
		out = append(out, statelog.Position{
			Stream: name, Generation: generation, Seq: info.State.LastSeq,
		})
	}
	return out
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
// a second list is how a new domain is silently left uncompared.
func replicatedDigest(t *testing.T, n *node) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, domain := range engine.Domains() {
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
// `internal/search` pins [search]'s partial-answer arithmetic against a fake
// peer returning nothing; only a downed route shows whether it can hang a
// search. The only case on [startPartitionableCluster]:
//  1. The scatter returns rather than waiting out the caller: blocking on an
//     absent member would stop every search on every node.
//  2. The silence is the partition: the same scatter is full BEFORE the cut.
//  3. And only the partition: the member the cut did not touch still answers.
//
// The scatter rides core NATS request/reply (`Ask`/`Serve` in [queue]).
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

	query := search.FanQuery{Text: "rollback drain node", Sources: []string{"page"},
		Methods: []search.Method{search.MethodLexical}}
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

	// ONE ASK, WHILE A WAIT BELOW WANTS EVERY PEER TO ANSWER, is short and
	// asked again rather than long. A healthy scatter on loopback answers in
	// milliseconds, so an ask that has not heard from every peer within a
	// second is waiting on a route rather than on an answer — and NATS
	// re-dials a lost route once a second (measured in this case's own log:
	// "attempt 1", "attempt 2" a second apart). An ask given clusterSettle
	// instead waited out all ten seconds when it went out a moment before
	// the route came back, before the next ask could find it. waitFor's
	// budget still bounds the whole wait.
	const askEach = time.Second

	// EVERY PEER ANSWERS FIRST. Without this the silence below is
	// unfalsifiable: a responder that never registered looks the same.
	waitFor(t, "every peer to answer a scatter", func() bool {
		got, _ := scatter(askEach)
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
		back, _ := scatter(askEach)
		return len(back) == len(peers)
	})
}

// A REFUSAL THAT REPEATS ENDS THE CASE; A LOST PORT IS RETRIED.
//
// The mesh retries by default, so what it must not retry is the short list
// known to repeat: a refused config, and a broker that declared itself dead.
// Before the broker's Fatalf was carried, a member whose store was refused
// waited out its readiness budget and was then reported as having lost its
// route port — so a store no attempt could fix was retried as a port race.
// A port taken during the start is still a death, and still the race.
func TestARefusalThatRepeatsIsNotRetried(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in        error
		retryable bool
	}{
		"a refused config": {&config.Fault{Kind: config.ErrShape, Detail: "not a port"}, false},
		"a store the broker refused": {
			fmt.Errorf("embedded nats server could not start: %w", jetstream.ErrBrokerDied), false},
		"a route port taken during the start": {
			fmt.Errorf("%w: ...: %w", jetstream.ErrRoutePortTaken, jetstream.ErrBrokerDied), true},
		"a leaf port taken during the start": {
			fmt.Errorf("%w: ...: %w", jetstream.ErrLeafPortTaken, jetstream.ErrBrokerDied), true},
		"a provision that timed out": {context.DeadlineExceeded, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := refusal("open backends", tc.in)
			if !errors.Is(out, tc.in) {
				t.Errorf("the refusal's own error was dropped: %v", out)
			}
			if got := !errors.Is(out, errNotRetryable); got != tc.retryable {
				t.Errorf("%v: retryable=%v, want %v", tc.in, got, tc.retryable)
			}
		})
	}
}
