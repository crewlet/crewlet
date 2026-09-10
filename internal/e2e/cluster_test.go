package e2e

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
	"github.com/crewlet/crewlet/internal/pages"
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
		id:          boot.Node.ID,
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
// is that two REAL engines — each with its own broker, its own store, its own
// index and its own registration — answer each other's slice requests. This is
// the only place the subject, the answerer, the assignment table and two
// independently built indexes are all live at once, and it is exactly the arm
// a single-node suite passes vacuously.
//
// # Why the assertion is on the COVERAGE and not only on the hits
//
// Both members hold the whole corpus, so neither NEEDS the other to answer. A
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

	// ENOUGH PAGES THAT EVERY MEMBER'S RANGE HOLDS SOME, and no more.
	// Each page is a real write through a quorum cluster and then an index
	// sweep, measured at a few seconds apiece here, so the count is chosen
	// against the probability it is there for: with 24 documents hashed
	// into 64 buckets, the chance that either contiguous half of the range
	// holds none of them is 2 x 2^-24 — about one run in four million.
	writer := c.nodes[0].engine.PagesStore()
	var last statelog.Position
	for i := range 24 {
		written, err := writer.Create(t.Context(), pageOperator(), pages.NewPage{
			Container: "ENG",
			Title:     fmt.Sprintf("Runbook %02d", i),
			Body:      "when a deploy hangs on rollback, drain the node before retrying",
		})
		if err != nil {
			t.Fatalf("create page %d: %v", i, err)
		}
		last = written.Outcome.Position
	}
	for i, n := range c.nodes {
		if err := n.engine.WaitCommitted(t.Context(), last); err != nil {
			t.Fatalf("member %d never applied the corpus: %v", i, err)
		}
		searcher := n.engine.NativeSearcher()
		if searcher == nil {
			t.Fatalf("member %d runs no native searcher", i)
		}
		waitFor(t, fmt.Sprintf("member %d's index to catch up", i), func() bool {
			return !searcher.Building(t.Context())
		})
	}

	table := search.Divide(c.nodeIDs())
	if len(table) != fleetSize {
		t.Fatalf("a fleet of %d divided into %d assignments", fleetSize, len(table))
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
					"[%d,%d) over a corpus of 24 pages that all match",
					i, slice.Node, slice.Shards.From, slice.Shards.To)
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
			tracker.DetailWants{}, statelog.ReadLinearizable)
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
	for i, n := range c.nodes {
		if err := n.engine.WaitCommitted(t.Context(), page.Outcome.Position); err != nil {
			t.Fatalf("member %d never applied the page: %v", i, err)
		}
		got, err := n.engine.Pages().Get(t.Context(), page.Page.ID)
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
	// only mean anything once both members have applied everything above.
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
