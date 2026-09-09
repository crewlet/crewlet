package e2e

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
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

// startCluster stands up n merged nodes on one clustered broker.
func startCluster(t *testing.T, n int) *cluster {
	t.Helper()
	if n < 2 {
		t.Fatalf("startCluster(%d): use start(t) for one node", n)
	}
	return startMesh(t, jetstreamtest.StartDirectMesh(t, n), n)
}

// startPartitionableCluster is [startCluster] on a mesh whose every route runs
// through a relay this process owns, so a case can cut one member off and put
// it back. Only the cases that partition take it — see
// [jetstreamtest.StartDirectMesh] for what it costs.
func startPartitionableCluster(t *testing.T, n int) *cluster {
	t.Helper()
	return startMesh(t, jetstreamtest.StartRelays(t, n), n)
}

func startMesh(t *testing.T, relays *jetstreamtest.Relays, n int) *cluster {
	t.Helper()
	for attempt := 1; attempt <= clusterStartAttempts; attempt++ {
		if c := startMeshOnce(t, relays, n, attempt); c != nil {
			return c
		}
	}
	// Unreachable: the last attempt fails the test rather than returning.
	return nil
}

func startMeshOnce(t *testing.T, relays *jetstreamtest.Relays, n, attempt int) *cluster {
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
			c.nodes[i], stops[i], errs[i] = buildMember(t, relays, i, n)
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
		return c
	}

	// EVERY MEMBER THIS ATTEMPT STARTED IS STOPPED BEFORE THE NEXT ONE,
	// and it is the difference between a retry and a pile-up.
	//
	// A member holds its cluster route PORT, which the relay mesh assigns
	// per member and reuses across attempts. Left running, it makes the
	// next attempt's member unable to bind — and any member that does come
	// up forms a cluster with the ghost, whose engine is still applying,
	// still heartbeating and still competing for the same four cores. The
	// observed shape was attempt 1 timing out on one member, attempt 2
	// failing to become ready at all, and attempt 3 failing worse: not
	// slowness, but each attempt racing everything the last one left.
	//
	// t.Cleanup cannot do this. It runs when the TEST ends, which is after
	// every attempt — so registering teardown there is registering it for
	// the wrong moment.
	stopAll(stops)
	if attempt < clusterStartAttempts {
		t.Logf("cluster attempt %d/%d lost a race at member %d: %v "+
			"(relays: %s)", attempt, clusterStartAttempts, failed, errs[failed],
			relays.Describe())
		return nil
	}
	t.Fatalf("member %d: %v (relays: %s)", failed, errs[failed], relays.Describe())
	return nil
}

// stopAll runs every member's teardown, in reverse order within each member.
//
// REVERSE, because that is the order the pieces were built in and each one's
// stop assumes the ones after it are still there: the projector reads the
// queue the engine owns, and the server serves the app.
func stopAll(stops [][]func()) {
	for _, member := range stops {
		for i := len(member) - 1; i >= 0; i-- {
			member[i]()
		}
	}
}

// clusterStartAttempts is how many times a fleet is stood up before the case
// gives up, and it is [jetstreamtest.clusterStartAttempts]'s reasoning applied
// one layer up: the collision is with work this process does not coordinate
// with, so a wider window does not help and noticing does.
const clusterStartAttempts = 3

// startMember brings up one member of the fleet.
//
// EVERY NODE HAS ITS OWN STORE AND ITS OWN STREAM DIRECTORY, because that is
// what a fleet is: n machines, each with its own disk. Sharing either would
// make this one node wearing three hats, and every fleet mechanism under it
// would pass for the wrong reason.
func buildMember(t *testing.T, relays *jetstreamtest.Relays, i, n int) (
	*node, []func(), error) {

	// THE TEARDOWN IS RETURNED, NOT REGISTERED WITH t.Cleanup, because an
	// attempt that fails has to stop what it started BEFORE the next one
	// starts — see [startMeshOnce]. It is built up as each piece comes up,
	// so a member that fails halfway still hands back a way to undo the
	// half that worked.
	var stops []func()
	fail := func(err error) (*node, []func(), error) { return nil, stops, err }

	model := newScriptedModel(t)
	cfg, err := config.ParseCompany([]byte(fmt.Sprintf(companyDoc, model.url)))
	if err != nil {
		return fail(fmt.Errorf("company config: %w", err))
	}

	port, peers, advertise := relays.Member(i)
	boot := config.DefaultBootstrap()
	boot.Node.ID = fmt.Sprintf("node-%d", i)
	boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	boot.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	boot.Stream.Cluster.Name = jetstreamtest.RelayClusterName
	boot.Stream.Cluster.Port = port
	boot.Stream.Cluster.Peers = peers
	// LOOPBACK, because the relays dial 127.0.0.1: a member listening on
	// every interface would be reachable on a port a partition does not
	// cut, and the partition would cut nothing.
	boot.Stream.Cluster.Host = "127.0.0.1"
	if advertise != "" {
		boot.Stream.Cluster.Advertise = advertise
	}
	// QUORUM-DURABLE, which is what makes "a write one node acknowledged
	// is a write every node will see" true rather than aspirational. At
	// one replica a publish is durable on the member that took it, and a
	// case asserting a peer sees it would be asserting timing.
	boot.Stream.Replicas = n

	e, err := engine.New(t.Context(), engine.Options{Bootstrap: &boot, Company: cfg})
	if err != nil {
		return fail(fmt.Errorf("engine.New: %w", err))
	}
	stops = append(stops, func() { e.Stop(context.Background()) })
	if err := e.Start(t.Context()); err != nil {
		return fail(fmt.Errorf("engine.Start: %w", err))
	}

	app := api.New(api.Options{
		Bootstrap:    &boot,
		QueueBackend: e.Backends().Queue.Backend(),
		Sources: queries.Sources{
			Events:  e.Backends().Store.Events(),
			Company: func() *config.Company { return cfg },
		},
		HealthInterval: tickInterval,
	})
	app.SetConfigured(true)
	app.Start(t.Context())
	stops = append(stops, app.Stop)

	projector := observe.NewProjector(e.Backends().Queue, app.Stream())
	if err := projector.Start(t.Context()); err != nil {
		return fail(fmt.Errorf("projector: %w", err))
	}
	stops = append(stops, func() { projector.Stop(context.Background()) })

	srv := httptest.NewServer(app)
	stops = append(stops, srv.Close)
	return &node{
		engine: e, app: app, server: srv, model: model,
		snapshotDir: boot.Store.SnapshotDirFor(),
	}, stops, nil
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
			n.engine.NativeHydrated)
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
// TWO, and the number is a cost decision rather than a coverage one. What a
// case here needs is a FLEET — two processes, two stores, two brokers, and
// agreement that has to cross between them — and two members gives all of it:
// a write one node makes reaches the other's rows through the log, and two
// nodes minting from one counter contend at the broker exactly as three would.
//
// What three would add is a MAJORITY, which is the only thing that makes
// "survive losing one" meaningful — and that is [jetstreamtest]'s own suite's
// subject, on bare brokers, where it costs three servers and not three
// engines. Here three members is three embedded brokers with better than
// twenty raft groups each, plus three pairs of SQLite databases, and measured
// on this repository's own CI shape they starve each other: each case passes
// alone and all of them time out under `go test ./...`.
const fleetSize = 2

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
				tracker.DetailWants{}, statelog.ReadSession)
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
		t.Errorf("three concurrent creates produced %d keys: %v", len(seen), seen)
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
	for _, want := range []string{"tracker", "vectors"} {
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
		t.Fatal("a fleet of three offered a joining node nothing — the donor " +
			"half is not running, so a node below the log's floor could never " +
			"adopt and would come up on the history it has for ever")
	}
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
