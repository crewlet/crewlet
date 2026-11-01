package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A NODE THAT FALLS BELOW THE FLOOR WHILE RUNNING ADOPTS WITHOUT A RESTART.
//
// # Why this is the case that matters
//
// Falling below the trim floor is what happens to a node that was paused,
// partitioned or slow for longer than the log's replay window — and such a
// node is RUNNING when it finds out. Until this existed the state was
// detected (its reads refused `below_floor`, its seats moved) and repaired
// only by a restart an operator had to know to perform; the join ran at boot
// and nowhere else.
//
// # The staging
//
// The node's appliers are halted so two records land on its log that it never
// applies, and the log is purged past them — which is exactly what the fleet's
// trim does to a node that has been away. A donor is stood up on the same
// broker holding a snapshot of the node's own rows at the purged position,
// which is what a peer that applied those two barriers would hold, since a
// barrier writes no rows. Then one heartbeat's publish is run, which is what
// notices: the heartbeat running it again on every tick is
// [TestTheHeartbeatPublishesOnEveryTickAndNudge]'s, and waiting here for its
// next ten-second tick certified nothing that case does not.
func TestANodeBelowTheFloorAdoptsWhileRunning(t *testing.T) {
	t.Parallel()
	e, back, q := bootRejoinNode(t)
	running, at, last := pushBelowTheFloor(t, e, q)

	// THE DONOR: this node's own rows at the purged position, which is
	// what a peer that applied those barriers holds.
	donorDir := t.TempDir()
	replicated := filepath.Join(donorDir, "crewlet-replicated.db")
	copyAdvancedTo(t, back, running, at, last, replicated)
	donorNode, _ := storetest.OpenEstate(t, filepath.Join(donorDir, "node.db"), store.Options{ReplicatedPath: replicated}, 1)
	t.Cleanup(func() { _ = donorNode.Close() })
	lag := uint64(0)
	var registered []statelog.Registered
	for _, domain := range registeredDomains() {
		registered = append(registered, statelog.Registered{
			Domain: domain, Spec: domain.Stream(),
			Health: func() statelog.Health { return statelog.Health{Drained: true, Lag: &lag} },
		})
	}
	snapDir := filepath.Join(donorDir, "snapshots")
	snapper, err := statelog.NewSnapshotter(statelog.SnapshotDeps{
		Domains: registered, File: donorNode.Replicated(), Dir: snapDir, NodeID: "donor",
		EngineVersion: "v0.0.0-test",
		Recipients:    func(context.Context) (int, error) { return 1, nil },
		Interval:      24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	manifest, err := snapper.Take(t.Context())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	donor, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: "donor",
		Dial:   func(context.Context) (*nats.Conn, error) { return q.DialOwned() },
		Newest: func() (statelog.Manifest, bool) { return manifest, true },
		Path: func(m statelog.Manifest) string {
			// THE NAME THE MANIFEST CARRIES, which is what the engine's
			// own donor does: a name derived here would be a fourth
			// independent derivation of what the file is called.
			return filepath.Join(snapDir, m.Artifact)
		},
	})
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	// A LIVE DATA NODE, as every donor is: the join waits for those.
	presentAsDataNode(t, e, "donor")
	donorCtx, stopDonor := context.WithCancel(t.Context())
	t.Cleanup(stopDonor)
	go func() { _ = donor.Serve(donorCtx) }()

	// THE HEARTBEAT NOTICES, the node adopts, and its appliers come back
	// over the artefact — with no restart and no operator.
	e.core.Load().log.publishPositions(t.Context())
	waitUntil(t, 90*time.Second, "the node to adopt the donor's snapshot", func() bool {
		return running.runner.Committed().Seq == last
	})
	// THE ADOPTION IS RECORDED AS COMPLETE, in this node's own estate and
	// naming this donor's artefact.
	var recorded string
	var completedAt sql.NullInt64
	if err := back.Store.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `
			SELECT manifest, completed_at FROM statelog_adoption
			ORDER BY started_at DESC LIMIT 1`).Scan(&recorded, &completedAt)
	}); err != nil {
		t.Fatalf("read the newest adoption row: %v", err)
	}
	if !completedAt.Valid {
		t.Fatal("the newest adoption row has no completed_at — the adoption " +
			"happened and the node serves on it, and its own record says it " +
			"stopped partway")
	}
	if recorded != manifest.SHA256 {
		t.Fatalf("the newest adoption row names artefact %s, want the donor's %s",
			recorded, manifest.SHA256)
	}
	// AND THE LEDGER LOST NOTHING: the ledger travels with the snapshot, so
	// the adopter holds its donor's rows and inherits its donor's watermark —
	// which says nothing lost, on a donor that never swept. An adopter that
	// recorded a loss here would answer its own backlog `unknown`.
	rows, err := worklet.NewRows(back.Store.Replicated().Reader(), worklet.Domain{}.Stream())
	if err != nil {
		t.Fatalf("build the tracker's read seam: %v", err)
	}
	if before, lost, err := rows.LostBefore(t.Context(), ""); err != nil || lost {
		t.Fatalf("the adopted ledger may have lost rows before %s (%v, %v) — a "+
			"ledger that travelled lost nothing, and every first attempt minted "+
			"before the join would be refused", before, lost, err)
	}
	// AND ITS APPLIERS CAME BACK OVER THE ARTEFACT: a write this node makes
	// is applied. Only a relaunched loop that got past its start check can
	// apply it, and that check is the one a wrong artefact fails — so this is
	// what holds the adoption to its promise. Its committed position says
	// nothing of the kind: Rejoined moves it before anything is relaunched.
	res, err := e.native.Load().writer.EvictNode(t.Context(), "op-after", "node-y")
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Position.Generation != at.Generation {
		t.Fatalf("a write after the adoption: %+v, %v (tracker applier stopped: %v) — "+
			"want it applied in generation %d", res, err, running.runner.Stopped(), at.Generation)
	}
	waitUntil(t, 30*time.Second, "the node to admit seats again", hydrated(t, e))
	if ok, domain := e.SeatsServiceable(t.Context()); !ok {
		t.Fatalf("the node cannot keep its seats after adopting: %s", domain)
	}
	// AND THE NEXT BEAT FINDS THE SAME HISTORY: a running node's heartbeat
	// compares the record its checkpoint names with the log's record there
	// every ten seconds, so an adoption the node only keeps until that beat
	// is no adoption. Run once rather than waited for, as above.
	e.core.Load().log.publishPositions(t.Context())
	if !e.StateLogHydrated(t.Context()) {
		t.Fatal("the node withdrew its seats at the first heartbeat after " +
			"adopting — the adopted checkpoint names another record than the " +
			"log holds at its sequence")
	}
	// AND THE CONSUMER WAS MOVED: nothing below the artefact's position is
	// pending for the applier to drop.
	pending, err := running.consumer.Pending(t.Context())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != 0 {
		t.Fatalf("%d record(s) pending after the adoption, want 0", pending)
	}
}

// A STOP THAT LANDS MID-REJOIN ENDS THE JOIN, AND WAITS FOR WHAT IT RELAUNCHED.
//
// # Why both halves
//
// A node that finds itself below the floor while running asks the fleet from a
// goroutine the state log's Stop waits for, and that goroutine starts the
// appliers again on its way out whatever the join concluded. Two things went
// wrong on that path. The ask ignored the context it was given, so a Stop sat
// out the rest of the offer window before anything else could shut down. And
// Stop halted the appliers BEFORE it waited — found them already halted by the
// rejoin, waited for the rejoin, and returned while the set the rejoin had
// just relaunched was still running against a store its caller closes next.
//
// # The staging
//
// A SILENT listener on the offer subject tells the case the moment the rejoin
// has asked, so the Stop lands inside the window rather than before the ask or
// after it. A join collects until every donor it names has answered — every
// live data node among them — so the window stays open only while one has
// not: a live data node is named here whose donor never answers — one not up
// yet, or gone — which is the window spent exactly as it would be in
// production. The heartbeat's publish is run once rather than waited for, as
// in [TestANodeBelowTheFloorAdoptsWhileRunning].
func TestAStopMidRejoinEndsTheJoinAndWaitsForItsAppliers(t *testing.T) {
	t.Parallel()
	e, _, q := bootRejoinNode(t)
	presentAsDataNode(t, e, "a-silent-peer")
	listener, err := q.DialOwned()
	if err != nil {
		t.Fatalf("dial the listener: %v", err)
	}
	t.Cleanup(listener.Close)
	asked := make(chan struct{}, 1)
	if _, err := listener.Subscribe(statelog.SubjectOffer, func(*nats.Msg) {
		select {
		case asked <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatalf("listen for asks: %v", err)
	}
	// BEFORE the node falls below the floor, so the first ask the
	// heartbeat makes is one this case hears.
	if err := listener.Flush(); err != nil {
		t.Fatalf("flush the listener: %v", err)
	}
	pushBelowTheFloor(t, e, q)
	e.core.Load().log.publishPositions(t.Context())

	select {
	case <-asked:
	case <-time.After(90 * time.Second):
		t.Fatal("the heartbeat never asked the fleet for a snapshot, so this " +
			"case has no rejoin to stop")
	}
	// PAST THE ASK'S FLUSH, so the Stop lands in the collection itself: the
	// listener hears the ask before the broker has answered the joiner's
	// flush, and a Stop inside that flush says nothing about the window.
	// An in-process broker answers a flush in microseconds.
	time.Sleep(200 * time.Millisecond)
	s := e.core.Load().log
	started := time.Now()
	s.Stop()
	took := time.Since(started)

	s.applyMu.Lock()
	relaunched := s.applyStop != nil
	s.applyMu.Unlock()
	if relaunched {
		t.Fatal("Stop returned with appliers the rejoin relaunched still " +
			"registered — nothing will ever join them, and they run against a " +
			"store the engine closes next")
	}
	// HALF THE WINDOW, which separates the two outcomes with room on both
	// sides: a Stop that sat the window out takes all of it less the
	// settle above, and one that cancelled it takes a context switch.
	if limit := statelog.OfferWindow / 2; took >= limit {
		t.Fatalf("Stop took %s mid-rejoin — the join sat out the offer window "+
			"(%s) rather than reading the context it was stopped through",
			took, statelog.OfferWindow)
	}
}

// bootRejoinNode starts one node over its own embedded broker and returns it
// with its backends and that broker.
func bootRejoinNode(t testing.TB) (*Engine, *Backends, *jetstream.Queue) {
	t.Helper()
	n := openRejoinNode(t)
	e := n.start(t)
	// PRESENT, as a running data node is from its seat host's start: a join
	// waits for the live data nodes' donors and no others, and this node's
	// own is one of them.
	presentAsDataNode(t, e, e.id)
	return e, n.back, n.q
}

// rejoinNode is the node [bootRejoinNode] stands up, before it is started: a
// case that has to reach its backends first — to put a register of its own in
// front of the fleet's — or boot the same node a second time over them, starts
// it itself.
type rejoinNode struct {
	boot    config.Bootstrap
	company *config.Company
	back    *Backends
	q       *jetstream.Queue
}

// openRejoinNode opens a [rejoinNode]'s backends, which the test closes.
func openRejoinNode(t testing.TB) *rejoinNode {
	t.Helper()
	return openRejoinNodeOver(t, nil)
}

// openRejoinNodeOver is [openRejoinNode] with wrap, when it is not nil, round
// the driver of the store the engine is handed ([store.Options.WrapDriver]) —
// which the node's replicated estate is opened through too, so a case can
// fail one step of an install as the store would.
//
// A SECOND HANDLE ON THE NODE'S FILE, beside the one [OpenBackends] opened:
// that one is what the queue's publish listener writes the event log through,
// and nothing detaches the listener, so closed early it would be written into
// on every publish. The store shares a path's claim within a process, and the
// first handle is closed after the queue that writes through it.
func openRejoinNodeOver(t testing.TB, wrap func(driver.Driver) driver.Driver) *rejoinNode {
	t.Helper()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	listened := back.Store
	if wrap != nil {
		opts := storeOptions(&b, cfg)
		opts.WrapDriver = wrap
		wrapped, openErr := store.OpenNode(t.Context(), b.Store.Path, opts)
		if openErr != nil {
			back.Close(context.Background())
			t.Fatalf("open the node's store through the wrapped driver: %v", openErr)
		}
		back.Store = wrapped
	}
	t.Cleanup(func() {
		back.Close(context.Background())
		if wrap != nil {
			_ = listened.Close()
		}
	})
	q, ok := back.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend", back.Queue)
	}
	return &rejoinNode{boot: b, company: cfg, back: back, q: q}
}

// start boots the node over its backends — again, for a case that stopped it —
// and stops it when the test ends.
func (n *rejoinNode) start(t testing.TB) *Engine {
	t.Helper()
	e, err := New(t.Context(), Options{Bootstrap: &n.boot, Company: n.company, Backends: n.back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e
}

// refusingHolds is a register whose every trim hold is refused, as a
// coordination store that cannot take this node's write refuses it: a fault of
// the joining node's own, which every offer would meet. asked counts the pins
// it refused, so a case can tell a join that met the fault from one that never
// asked.
type refusingHolds struct {
	fleetRegister
	asked atomic.Int64
}

func (r *refusingHolds) PutHold(context.Context, coord.TrimHold) error {
	r.asked.Add(1)
	return errors.New("the coordination store refused this node's write")
}

// refusingPositions is a register whose positions read is refused once armed,
// as a coordination store that cannot be reached refuses it — the first fleet
// read a join makes, before it asks anybody for anything.
type refusingPositions struct {
	fleetRegister
	armed atomic.Bool
}

// errPositionsRefused is what an armed [refusingPositions] answers, so a case
// can tell the refusal it staged from a failure the fixture made by accident.
var errPositionsRefused = errors.New("the coordination store refused the positions read")

func (r *refusingPositions) Positions(ctx context.Context) ([]coord.NodePositions, error) {
	if r.armed.Load() {
		return nil, errPositionsRefused
	}
	return r.fleetRegister.Positions(ctx)
}

// refuseInstalled is a driver wrapper ([store.Options.WrapDriver]) that, once
// armed on the node's replicated file, refuses a connection to whatever file
// is put at that path in its place — as a store that cannot open what it was
// handed refuses it — and lets every connection to the file it was armed on
// through. So a join over it reads, closes and replaces its estate as it
// always does, and fails at exactly one step: opening the artefact its install
// renamed into place.
type refuseInstalled struct {
	mu      sync.Mutex
	path    string
	armedOn os.FileInfo

	// refused counts the connections it turned away, so a case can tell a
	// join that met the failure from one that never got as far as the
	// install.
	refused atomic.Int64
}

// errInstalledRefused is what [refuseInstalled] refuses a connection with.
var errInstalledRefused = errors.New("the store refused the file installed at this path")

// arm starts refusing any file put at path in place of the one there now.
func (r *refuseInstalled) arm(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("read the file to arm on: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.path, r.armedOn = path, info
}

// replaced reports whether a connection to name would reach a file put in
// place of the one r was armed on.
func (r *refuseInstalled) replaced(name string) bool {
	r.mu.Lock()
	path, armedOn := r.path, r.armedOn
	r.mu.Unlock()
	if armedOn == nil || name != path {
		return false
	}
	now, err := os.Stat(name)
	return err == nil && !os.SameFile(now, armedOn)
}

// wrap is the [store.Options.WrapDriver] function: a method, so the value a
// case arms is the one the store connects through.
func (r *refuseInstalled) wrap(d driver.Driver) driver.Driver {
	return refuseInstalledDriver{inner: d, r: r}
}

type refuseInstalledDriver struct {
	inner driver.Driver
	r     *refuseInstalled
}

// Open hands back the inner driver's own connection, unwrapped, so every
// connection it lets through is exactly the one the store would have made.
func (d refuseInstalledDriver) Open(name string) (driver.Conn, error) {
	if d.r.replaced(name) {
		d.r.refused.Add(1)
		return nil, errInstalledRefused
	}
	return d.inner.Open(name)
}

// trackerCheckpoint is the tracker's checkpoint in the node's replicated file.
func trackerCheckpoint(t *testing.T, e *Engine) statelog.Position {
	t.Helper()
	at, _, found, err := statelog.CursorFor(t.Context(),
		e.backends.Store.Replicated().Reader(), worklet.Domain{}.Stream().Name)
	if err != nil || !found {
		t.Fatalf("read the tracker's checkpoint: found %v, %v", found, err)
	}
	return at
}

// pushBelowTheFloor leaves a running node below its tracker log's floor, which
// is exactly what the fleet's trim does to a node that has been away: its
// appliers stop, two barriers land on the log that it never applies, and the
// log is purged up to the second of them. It returns the tracker's running
// domain, the position the node had applied, and the last sequence appended —
// the position a peer that applied those barriers would hold, and, since a
// purge at N leaves N itself, the log's first record.
func pushBelowTheFloor(t *testing.T, e *Engine, q *jetstream.Queue) (
	running *runningLog, at statelog.Position, last uint64) {

	t.Helper()
	running, at, log, last := appendPastTheNode(t, e, q)
	if err := log.Purge(t.Context(), last); err != nil {
		t.Fatalf("purge: %v", err)
	}
	return running, at, last
}

// appendPastTheNode stops a running node's appliers and lands two barriers on
// its tracker log that it never applies — with nothing purged, so the log
// still holds what the node lacks. It returns the tracker's running domain,
// the position the node had applied, the log, and the last sequence appended:
// the position a peer that applied those barriers would hold.
func appendPastTheNode(t *testing.T, e *Engine, q *jetstream.Queue) (
	running *runningLog, at statelog.Position, log *jetstream.DomainLog, last uint64) {

	t.Helper()
	spec := worklet.Domain{}.Stream()
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	s := e.core.Load().log
	running = s.Domain(worklet.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}

	// Left running, the collector's boot pass pins between the two staged
	// below and moves where they land.
	quietTheCollector(t, e)

	body, err := worklet.EncodeBarrier(statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind}, Gen: running.runner.Committed().Generation,
		Scope: statelog.ScopeSet{Paths: []string{statelog.FrameworkScope}},
	})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	// THE NODE HAS APPLIED HISTORY of its own before it falls behind: one
	// barrier it applies, so its tracker rows hold a checkpoint, as every
	// node that has been away does. Stated here rather than left to the
	// collector's first pass, which writes no barrier when it has nothing
	// to judge.
	if _, _, err := log.Append(t.Context(), spec.SubjectPrefix+"."+statelog.BarrierKind,
		"", nil, running.signer.Seal(body)); err != nil {
		t.Fatalf("append the node's own barrier: %v", err)
	}

	// Settle: whatever was written to the tracker's log is applied.
	waitUntil(t, 20*time.Second, "the node to catch up on its own log", func() bool {
		_, end, err := log.Bounds(t.Context())
		return err == nil && running.runner.Committed().Seq == end
	})
	at = running.runner.Committed()

	s.haltAppliers()
	// AND NOTHING OF IT IS STILL WAITING ON THE LOG. The halted loop's
	// last fetch leaves its request standing on the broker for the rest of
	// its wait, and a record appended into it is DELIVERED — to the
	// consumer's standing buffer, where the next loop on this handle takes
	// it, which is the whole point of that buffer. A node that has been away
	// long enough to fall below a floor has nothing standing, so the
	// barriers below wait until nothing is.
	waitUntil(t, 20*time.Second, "the halted loop's last request to end", func() bool {
		cons, err := q.JetStream().Consumer(t.Context(), spec.Name, running.consumer.Name())
		if err != nil {
			return false
		}
		info, err := cons.Info(t.Context())
		return err == nil && info.NumWaiting == 0
	})
	for range 2 {
		if last, _, err = log.Append(t.Context(), spec.SubjectPrefix+"."+statelog.BarrierKind,
			"", nil, running.signer.Seal(body)); err != nil {
			t.Fatalf("append a barrier: %v", err)
		}
	}
	if last != at.Seq+2 {
		t.Fatalf("the barriers landed at %d, want %d", last, at.Seq+2)
	}
	return running, at, log, last
}

// copyAdvancedTo writes a copy of the node's replicated estate to path with its
// tracker checkpoint advanced to last — what a peer holds that applied the
// barriers past at, since a barrier writes no rows — and quiesces it, so the
// file is self-contained for whatever opens or installs it.
//
// THE CHECKPOINT NAMES THE RECORD AT last, by the instant the broker stored it,
// as a peer's commit of that barrier wrote it ([moveCheckpoint]). Advancing the
// sequence alone left it naming the node's own record at at — another record
// than the log holds at last, which the purge keeps — so the adopter's first
// heartbeat found the log diverged from the rows it had just installed and
// took its copy out of service, and a case that read the node before that
// beat passed on the race.
func copyAdvancedTo(t *testing.T, back *Backends, running *runningLog,
	at statelog.Position, last uint64, path string) {

	t.Helper()
	if _, err := storetest.ReplicatedDB(t, back.Store.Replicated()).Backup(t.Context(), path); err != nil {
		t.Fatalf("copy the replicated estate: %v", err)
	}
	copyDB, err := store.OpenEstate(t.Context(), store.EstateReplicated, path, store.Options{})
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	moveCheckpoint(t, running.log, copyDB, statelog.Position{
		Stream: worklet.Domain{}.Stream().Name, Generation: at.Generation, Seq: last,
	}, running.runner.StreamCreatedAt())
	if err := copyDB.Close(); err != nil {
		t.Fatalf("close the copy: %v", err)
	}
	if err := store.QuiesceCopy(t.Context(), path); err != nil {
		t.Fatalf("quiesce the copy: %v", err)
	}
}

// quietTheCollector lets the object collector finish its first pass and stops
// it, so nothing but the case itself appends to the tracker's log from here.
//
// A PASS THAT HAS AN OBJECT TO JUDGE PINS THE TRACKER'S ESTATE WITH A BARRIER
// of its own, and the first runs as the node boots, on its own goroutine, whenever its duty claim and
// its decline check come back — under load, now and then after the node
// admits seats and after a case's first write. A case that reads the log's
// end, or stages records at chosen sequences, around a pin landing where it
// pleases is reading a log it does not know: a probe of the restored reanchor
// case caught the pin 46 ms after the case's own write, between its copy of
// the estate and its read of the checkpoint, and the donor it stood up named
// a record the log did not hold at that sequence. Stopping it mid-pass is not
// enough: a barrier's publish outlives the read that asked for it, so the
// pass is waited out first.
func quietTheCollector(t testing.TB, e *Engine) {
	t.Helper()
	waitUntil(t, 20*time.Second, "the collector's first pass to be recorded", func() bool {
		_, found, err := e.backends.ObjectCollection(t.Context())
		return err == nil && found
	})
	e.stopObjectCollector()
}

// moveCheckpoint moves db's checkpoint on at's stream to at, keyed to created,
// NAMING THE RECORD log holds at that sequence by its broker instant, as every
// checkpoint an applier commits does (statelog's setCursor): a checkpoint is a
// record, not only a sequence. Moved by its sequence alone, a checkpoint goes
// on naming the record it stood on before; the applier that loads it finds
// another record there and stops, rightly, as diverged, and the staged rows
// are ones no peer could hold. A case passed on them only while its waits beat
// that stop. A checkpoint at sequence zero, or at one the log does not hold —
// below a rebuilt log's first record, where a reanchor puts it — names no
// record, as a reanchor's own checkpoint does.
func moveCheckpoint(t *testing.T, log *jetstream.DomainLog, db *store.DB,
	at statelog.Position, created time.Time) {

	t.Helper()
	var stored int64
	if at.Seq > 0 {
		_, _, storedAt, held, err := log.At(t.Context(), at.Seq)
		if err != nil {
			t.Fatalf("read the log's record at the checkpoint %s: %v", at, err)
		}
		if held {
			stored = store.EncodeTime(storedAt)
		}
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, stored_at, updated_at, applied_version)
			VALUES (?, ?, ?, ?, ?, ?, 0)
			ON CONFLICT (stream) DO UPDATE SET
				generation = excluded.generation, seq = excluded.seq,
				stream_created_at = excluded.stream_created_at, stored_at = excluded.stored_at`,
			at.Stream, int64(at.Generation), int64(at.Seq), store.EncodeTime(created), stored,
			store.EncodeTime(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("move the copy's checkpoint to %s: %v", at, err)
	}
}

func waitUntil(t testing.TB, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waited %s for %s", within, what)
}

// A LOST ESTATE IS REOPENED, AND ONLY THEN IS ANYTHING ASKED OF IT.
//
// A join whose install failed and whose rollback could not reopen the live
// database — or one that installed an artefact and could not open it — leaves
// the node with no replicated estate at all. Nothing else in a running node
// reopens one: its appliers retry against ErrNoEstate, its reads refuse, and
// every step of a join reads the estate that is gone. So the heartbeat's
// restore opens it, and the rejoin then asks the ordinary question of what it
// opened.
//
// Both answers are staged, each as a failed join leaves a node — appliers
// halted, estate closed — with the heartbeat's own requests switched off so the
// calls under test are the only ones. Below the floor, a lone node then finds
// no donor, which is only reachable by a join that ran over an open estate.
// With nothing missing — the state an earlier rejoin leaves when the artefact
// it installed is what failed to open — the restore alone is the recovery.
//
// AND A REOPENED ESTATE NAMES EVERYONE TO THE LISTENER, because the file it
// opened may be a donor's whose revocations no committed batch here ever named
// — while a rejoin that found nobody to ask changed no row and says nothing,
// which is the control.
func TestALostEstateIsReopenedBeforeAnythingIsAskedOfIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		below bool
	}{
		{"below the floor", true},
		{"with nothing missing", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e, back, q := bootRejoinNode(t)
			s := e.core.Load().log
			quietHeartbeat(s)
			if c.below {
				pushBelowTheFloor(t, e, q)
			} else {
				s.haltAppliers()
			}
			if err := back.Store.CloseReplicated(); err != nil {
				t.Fatalf("close the replicated estate: %v", err)
			}
			heard := &heardMoves{}
			e.SetOnIdentityMoved(heard.hear)
			// WHILE IT IS SHUT THE COPY IS NOT SERVED: every read of it
			// fails, and with the appliers halted on a log nobody writes
			// nothing else would ever call it wrong — so this node's own
			// seats would be answered that failure rather than asked of
			// a peer.
			if v := s.judgeCopy(t.Context()); !v.shut || v.answers(worklet.Domain{}.Name()) {
				t.Fatalf("a copy whose file is shut is judged %+v, want it shut — "+
					"every domain's copy out of service at once", v)
			}

			if err := s.restoreEstate(s.run); err != nil {
				t.Fatalf("restore: %v", err)
			}
			if !replicatedOpen(back.Store) {
				t.Fatal("the restore left the replicated estate closed — nothing " +
					"else in a running node reopens it")
			}
			if got := heard.take(); !slices.ContainsFunc(got,
				func(m iamdomain.Moved) bool { return m.Everyone }) {
				t.Errorf("a reopened estate told the credential listener %+v, "+
					"want everyone: its rows may be a donor's", got)
			}
			if v := s.judgeCopy(t.Context()); v.shut {
				t.Fatalf("a copy whose file the restore reopened is still judged shut: %+v", v)
			}
			if !c.below {
				return
			}
			if err := e.rejoin(s.run, s); !errors.Is(err, errNoDonor) {
				t.Fatalf("rejoin over the restored estate = %v, want %v — a lone "+
					"node below the floor has nobody to ask, which only a join "+
					"that could read its own checkpoints gets as far as learning",
					err, errNoDonor)
			}
			if got := heard.take(); slices.ContainsFunc(got,
				func(m iamdomain.Moved) bool { return m.Everyone }) {
				t.Errorf("a rejoin that found no donor told the credential "+
					"listener %+v: it changed no row", got)
			}
		})
	}
}

// A JOIN NOBODY CAN DONATE TO ENDS ON THE FLEET'S ANSWERS, NOT ON THE WINDOW.
//
// A node below the floor refuses every read and write while it asks, and it
// asked for the whole offer window whatever the fleet said: a donor holding
// nothing stayed silent, and every data node runs a donor — the asking node's
// own among them. So a lone node below the floor spent five seconds refusing
// everything to hear itself say nothing. Its donor now declines, and the join
// waits for the live data nodes it lists and no longer.
func TestALoneNodesJoinEndsOnItsOwnDonorsAnswer(t *testing.T) {
	t.Parallel()
	e, _, q := bootRejoinNode(t)
	s := e.core.Load().log
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	quietHeartbeat(s)
	pushBelowTheFloor(t, e, q)
	started := time.Now()
	if err := e.rejoin(s.run, s); !errors.Is(err, errNoDonor) {
		t.Fatalf("a lone node's rejoin = %v, want %v", err, errNoDonor)
	}
	// HALF THE WINDOW, which separates the two outcomes with room on both
	// sides: a join that waited for the window takes all of it, and one
	// that stopped on its own donor's decline takes a round trip and the
	// reads around it.
	if took := time.Since(started); took >= statelog.OfferWindow/2 {
		t.Fatalf("a lone node's join took %v — it waited out the %v offer "+
			"window rather than stopping on its own donor's answer",
			took, statelog.OfferWindow)
	}
}

// quietHeartbeat stops the heartbeat requesting anything of s, so a case that
// drives the restore or the rejoin itself is the only caller.
func quietHeartbeat(s *stateLog) {
	s.rejoinMu.Lock()
	defer s.rejoinMu.Unlock()
	s.rejoin = nil
}

// AN INSTALLED ARTEFACT MOVES THE CONSUMERS, WHICHEVER STEP OPENS IT.
//
// A join that installs an artefact and then cannot open it leaves the DONOR'S
// file under the node, at a checkpoint no consumer was ever moved to. Two
// steps open it afterwards: the heartbeat's restore, and a rejoin that runs
// over it once it is open and finds every domain CURRENT, because the artefact
// is. Each must move every consumer to the file's checkpoint, or the broker
// delivers everything in between for the applier to drop one at a time.
//
// The artefact is ahead of the node by two records the log still holds, so a
// join over it finds it current rather than behind. What is read is the
// broker's own consumer configuration, because only a reset writes it: an
// applier relaunched over the file drains and drops the stale records either
// way, and the runner's position comes from the checkpoint rather than from
// the consumer — so neither says whether the consumer moved.
func TestAnInstalledArtefactMovesTheConsumersWhicheverStepOpensIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		open func(t *testing.T, e *Engine, back *Backends, s *stateLog) error
	}{
		{"the restore", func(_ *testing.T, _ *Engine, _ *Backends, s *stateLog) error {
			return s.restoreEstate(s.run)
		}},
		{"a rejoin that finds it current", func(t *testing.T, e *Engine, back *Backends, s *stateLog) error {
			if _, err := back.Store.OpenReplicated(t.Context(), estateLogs()); err != nil {
				t.Fatalf("open the installed artefact: %v", err)
			}
			return e.rejoin(s.run, s)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e, back, q := bootRejoinNode(t)
			s := e.core.Load().log
			quietHeartbeat(s)
			running, at, _, last := appendPastTheNode(t, e, q)
			artefact := filepath.Join(t.TempDir(), "artefact.db")
			copyAdvancedTo(t, back, running, at, last, artefact)
			// WHAT A JOIN LEAVES when it installed an artefact and could
			// not open it: the file renamed into place, the estate closed.
			if err := back.Store.CloseReplicated(); err != nil {
				t.Fatalf("close the replicated estate: %v", err)
			}
			if err := store.AdoptFile(t.Context(), back.Store.ReplicatedFile(), artefact); err != nil {
				t.Fatalf("install the artefact: %v", err)
			}

			if err := c.open(t, e, back, s); err != nil {
				t.Fatalf("%s over the installed artefact: %v", c.name, err)
			}
			js, err := natsjs.New(q.Conn())
			if err != nil {
				t.Fatalf("open a JetStream handle: %v", err)
			}
			cons, err := js.Consumer(t.Context(), worklet.Domain{}.Stream().Name,
				running.consumer.Name())
			if err != nil {
				t.Fatalf("look up the tracker's consumer: %v", err)
			}
			info, err := cons.Info(t.Context())
			if err != nil {
				t.Fatalf("read the tracker's consumer: %v", err)
			}
			if got := info.Config; got.DeliverPolicy != natsjs.DeliverByStartSequencePolicy ||
				got.OptStartSeq != last+1 {
				t.Fatalf("the consumer starts %s at %d, want at %d — the file's "+
					"checkpoint is %d, and a consumer left where the replaced file "+
					"had it delivers every record in between to be dropped",
					got.DeliverPolicy, got.OptStartSeq, last+1, last)
			}
		})
	}
}

// A REJOIN WHOSE OWN RECORD FAILS AFTER THE INSTALL IS STILL AN ADOPTION.
//
// The rename is the join's point of no return: from it the donor's file is the
// live database, open, with every applier resuming from its checkpoint. What
// can still fail is this node's HISTORY row of the adoption, in the node
// estate — a different file, with writers of its own while the node runs.
// Reported as "no donor", the rejoin skipped everything an adoption owes: no
// consumer was moved to the file's checkpoint, and no listener was told the
// rows were replaced, so a dashboard socket a donor's rows had revoked kept
// streaming the company — and the node, now current, never asked again, so
// nothing ever told them.
//
// The row's advance is refused by a trigger on the node estate, which is the
// store failing a write it was asked for and nothing else.
func TestARejoinWhoseRecordFailsAfterTheInstallIsAnAdoption(t *testing.T) {
	t.Parallel()
	e, back, q := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats",
		func() bool { return e.StateLogHydrated(t.Context()) })
	s := e.core.Load().log
	quietHeartbeat(s)
	running, at, last := pushBelowTheFloor(t, e, q)
	rows := filepath.Join(t.TempDir(), "crewlet-replicated.db")
	copyAdvancedTo(t, back, running, at, last, rows)
	standUpDonor(t, e, q, rows, statelog.Position{
		Stream: at.Stream, Generation: at.Generation, Seq: last,
	}, running.runner.KeyedTo())

	// THE INSTALLED PHASE IS THE FIRST UPDATE OF THE ROW the verified
	// phase inserted, so refusing every update refuses both phases after
	// the rename and nothing before it.
	if err := back.Store.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TRIGGER refuse_the_adoption_row
			BEFORE UPDATE ON statelog_adoption
			BEGIN SELECT RAISE(ABORT, 'the node estate refused the write'); END`)
		return err
	}); err != nil {
		t.Fatalf("install the trigger: %v", err)
	}

	heard := &heardMoves{}
	e.SetOnIdentityMoved(heard.hear)
	if err := e.rejoin(s.run, s); err != nil {
		t.Fatalf("rejoin = %v, want an adoption — the donor's file is the live "+
			"database and only this node's history row of it is short", err)
	}
	if got := heard.take(); !slices.ContainsFunc(got,
		func(m iamdomain.Moved) bool { return m.Everyone }) {
		t.Fatalf("the credential listener heard %+v, want everyone: the rows are "+
			"a donor's, and a socket they revoked would go on streaming", got)
	}
	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("open a JetStream handle: %v", err)
	}
	cons, err := js.Consumer(t.Context(), worklet.Domain{}.Stream().Name, running.consumer.Name())
	if err != nil {
		t.Fatalf("look up the tracker's consumer: %v", err)
	}
	info, err := cons.Info(t.Context())
	if err != nil {
		t.Fatalf("read the tracker's consumer: %v", err)
	}
	if got := info.Config; got.DeliverPolicy != natsjs.DeliverByStartSequencePolicy ||
		got.OptStartSeq != last+1 {
		t.Fatalf("the consumer starts %s at %d, want at %d — an adoption moves "+
			"every consumer to the file's checkpoint", got.DeliverPolicy, got.OptStartSeq, last+1)
	}
	// AND THE ROW SAYS WHAT HAPPENED TO IT: begun, never advanced.
	var completed sql.NullInt64
	if err := back.Store.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT completed_at FROM statelog_adoption
			ORDER BY started_at DESC LIMIT 1`).Scan(&completed)
	}); err != nil {
		t.Fatalf("read the adoption row: %v", err)
	}
	if completed.Valid {
		t.Fatal("the adoption row is complete although every write advancing it " +
			"was refused")
	}
}

// A REJOIN THAT MET A FAULT OF THIS NODE'S OWN REPLACED NOTHING, AND SAYS SO.
//
// The coordination store refuses the pin on the log's tail, which every offer
// would meet: the join ends there, nothing was installed, and the live
// database was never closed. So the rejoin must not announce the estate
// replaced — that closes every dashboard socket on the node and resets every
// artifact room, and it would do it again on every retry for as long as the
// fault lasted — and must answer as a node still behind, so its retry widens.
func TestARejoinThatMetAFaultOfItsOwnReplacesNothing(t *testing.T) {
	t.Parallel()
	n := openRejoinNode(t)
	holds := &refusingHolds{fleetRegister: n.back.Fleet}
	n.back.Fleet = holds
	e := n.start(t)
	presentAsDataNode(t, e, e.id)
	waitUntil(t, 20*time.Second, "the node to admit seats",
		func() bool { return e.StateLogHydrated(t.Context()) })
	s := e.core.Load().log
	quietHeartbeat(s)
	running, at, last := pushBelowTheFloor(t, e, n.q)
	rows := filepath.Join(t.TempDir(), "crewlet-replicated.db")
	copyAdvancedTo(t, n.back, running, at, last, rows)
	standUpDonor(t, e, n.q, rows, statelog.Position{
		Stream: at.Stream, Generation: at.Generation, Seq: last,
	}, running.runner.KeyedTo())

	heard := &heardMoves{}
	e.SetOnIdentityMoved(heard.hear)
	err := e.rejoin(s.run, s)
	if !errors.Is(err, errJoinUnable) {
		t.Fatalf("rejoin = %v, want %v — a node whose own pin was refused is "+
			"behind, as when nobody could donate, and retries", err, errJoinUnable)
	}
	if holds.asked.Load() == 0 {
		t.Fatal("the join never asked for its pin, so nothing here met the fault")
	}
	if got := heard.take(); slices.ContainsFunc(got,
		func(m iamdomain.Moved) bool { return m.Everyone }) {
		t.Fatalf("the credential listener heard %+v: a join that installed nothing "+
			"announced the rows replaced, closing every socket on the node", got)
	}
	if got := trackerCheckpoint(t, e); got.Seq != at.Seq {
		t.Fatalf("the tracker's rows stand at %d after a join that installed "+
			"nothing, want the %d they held", got.Seq, at.Seq)
	}
}

// A REJOIN THAT FAILED BEFORE IT ASKED ANYBODY REPLACED NOTHING, AND SAYS
// NOTHING.
//
// The join's first fleet read — the positions register, which names the
// generation each log is on — is refused, as a coordination store that cannot
// be reached refuses it. Nothing was asked, fetched or installed, and the live
// database was never closed, so the rejoin fails with that refusal and must
// not announce the estate replaced: that closes every dashboard socket on the
// node and resets every artifact room, and it would do it again on every
// retry for as long as the store stayed out of reach. The one failure that
// does announce it is [TestARejoinThatCouldNotOpenWhatItInstalledSaysTheRowsWereReplaced]'s.
func TestARejoinThatFailedBeforeItAskedReplacesNothing(t *testing.T) {
	t.Parallel()
	n := openRejoinNode(t)
	positions := &refusingPositions{fleetRegister: n.back.Fleet}
	n.back.Fleet = positions
	e := n.start(t)
	presentAsDataNode(t, e, e.id)
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	s := e.core.Load().log
	quietHeartbeat(s)
	_, at, _ := pushBelowTheFloor(t, e, n.q)

	heard := &heardMoves{}
	e.SetOnIdentityMoved(heard.hear)
	positions.armed.Store(true)
	err := e.rejoin(s.run, s)
	positions.armed.Store(false)
	if !errors.Is(err, errPositionsRefused) {
		t.Fatalf("rejoin = %v, want the refused positions read — the staging "+
			"never reached the join's first fleet read", err)
	}
	if errors.Is(err, statelog.ErrEstateNotRestored) {
		t.Fatalf("rejoin = %v: a join that failed before it asked anybody "+
			"reports the estate lost", err)
	}
	if got := heard.take(); slices.ContainsFunc(got,
		func(m iamdomain.Moved) bool { return m.Everyone }) {
		t.Fatalf("the credential listener heard %+v: a join that failed before "+
			"it asked anybody announced the rows replaced, closing every socket "+
			"on the node", got)
	}
	if !replicatedOpen(n.back.Store) {
		t.Fatal("the replicated estate is closed after a join that never got as " +
			"far as closing it")
	}
	if got := trackerCheckpoint(t, e); got.Seq != at.Seq {
		t.Fatalf("the tracker's rows stand at %d after a join that installed "+
			"nothing, want the %d they held", got.Seq, at.Seq)
	}
}

// A REJOIN THAT COULD NOT OPEN WHAT IT INSTALLED SAYS THE ROWS WERE REPLACED.
//
// The install renamed the donor's file to the live path and the store would
// not open it, so the node holds no replicated estate — and the file under it
// is the donor's, whose revocations no committed batch here ever named. That
// is [statelog.ErrEstateNotRestored], and it is the one failed join that
// announces the rows replaced: the heartbeat's restore says it again once it
// opens the file, which is a heartbeat away at the soonest and never while
// the cause lasts, and until then nothing else names a socket the donor's
// rows ended. The control is [TestARejoinThatFailedBeforeItAskedReplacesNothing].
func TestARejoinThatCouldNotOpenWhatItInstalledSaysTheRowsWereReplaced(t *testing.T) {
	t.Parallel()
	refuse := &refuseInstalled{}
	n := openRejoinNodeOver(t, refuse.wrap)
	e := n.start(t)
	presentAsDataNode(t, e, e.id)
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	s := e.core.Load().log
	quietHeartbeat(s)
	running, at, last := pushBelowTheFloor(t, e, n.q)
	rows := filepath.Join(t.TempDir(), "crewlet-replicated.db")
	copyAdvancedTo(t, n.back, running, at, last, rows)
	standUpDonor(t, e, n.q, rows, statelog.Position{
		Stream: at.Stream, Generation: at.Generation, Seq: last,
	}, running.runner.KeyedTo())

	heard := &heardMoves{}
	e.SetOnIdentityMoved(heard.hear)
	refuse.arm(t, n.back.Store.ReplicatedFile())
	err := e.rejoin(s.run, s)
	if !errors.Is(err, statelog.ErrEstateNotRestored) {
		t.Fatalf("rejoin = %v, want %v — the artefact is installed and the "+
			"store would not open it", err, statelog.ErrEstateNotRestored)
	}
	if refuse.refused.Load() == 0 {
		t.Fatal("the store never turned the installed file away, so the join " +
			"failed somewhere else and nothing here met the failure")
	}
	if replicatedOpen(n.back.Store) {
		t.Fatal("the replicated estate is open after a join whose install the " +
			"store would not open")
	}
	if got := heard.take(); !slices.ContainsFunc(got,
		func(m iamdomain.Moved) bool { return m.Everyone }) {
		t.Fatalf("the credential listener heard %+v, want everyone: the file "+
			"at the live path is the donor's, and a socket its rows ended would "+
			"go on streaming until the restore opens it", got)
	}
}

// A BOOT WHOSE JOIN MET A FAULT OF ITS OWN COMES UP BEHIND.
//
// The same refused pin at boot. The node's own database is open and untouched
// — the join never reached the install — so it is exactly a node nobody could
// donate to: it comes up on the history it has, its coverage and seat admission
// say what it cannot answer for, and it asks again on its widening interval.
// Stopping the boot on it would make a coordination store that refused one
// write a process exit, and a fault that lasts a crash loop paying the broker's
// recovery on every attempt.
func TestABootWhoseJoinMetAFaultOfItsOwnComesUpBehind(t *testing.T) {
	t.Parallel()
	n := openRejoinNode(t)
	e := n.start(t)
	presentAsDataNode(t, e, e.id)
	waitUntil(t, 20*time.Second, "the node to admit seats",
		func() bool { return e.StateLogHydrated(t.Context()) })
	quietHeartbeat(e.core.Load().log)
	running, at, last := pushBelowTheFloor(t, e, n.q)
	rows := filepath.Join(t.TempDir(), "crewlet-replicated.db")
	copyAdvancedTo(t, n.back, running, at, last, rows)
	standUpDonor(t, e, n.q, rows, statelog.Position{
		Stream: at.Stream, Generation: at.Generation, Seq: last,
	}, running.runner.KeyedTo())
	e.Stop(context.Background())

	// THE SAME NODE BOOTS AGAIN, below the floor, with a donor waiting and
	// its own pin refused.
	holds := &refusingHolds{fleetRegister: n.back.Fleet}
	n.back.Fleet = holds
	again, err := New(t.Context(), Options{Bootstrap: &n.boot, Company: n.company, Backends: n.back})
	if err != nil {
		t.Fatalf("New = %v, want a node that comes up behind — its own database "+
			"is open and untouched, and the fault is one it can retry past", err)
	}
	t.Cleanup(func() { again.Stop(context.Background()) })
	if holds.asked.Load() == 0 {
		t.Fatal("the boot never asked for its pin, so it never joined and " +
			"nothing here met the fault")
	}
	if got := trackerCheckpoint(t, again); got.Seq != at.Seq {
		t.Fatalf("the tracker's rows stand at %d after a join that installed "+
			"nothing, want the %d they held", got.Seq, at.Seq)
	}
}

// A LOST ESTATE IS REOPENED ON THE NEXT HEARTBEAT; THE FLEET IS ASKED ON ITS
// OWN INTERVAL.
//
// The widening pause after a failed rejoin exists to bound how often the fleet
// is asked, each ask an offer window the node spends refusing — and one that
// finds a donor, a whole artefact's transfer. A reopen of this node's own
// database asks nobody, and the node serves nothing until it succeeds. So the
// two run on two schedules, and each case pins one edge between them:
//
//   - an ask that found no donor widens the pause, and the next heartbeat asks
//     nobody;
//   - an ask that lost the estate on its way out widens it too — it fetched,
//     and the next ask would fetch again — while the next heartbeat still
//     reopens the estate, and asks nothing;
//   - a reopen that fails moves no schedule, and the heartbeat after the cause
//     is gone reopens the estate and only then asks the fleet.
//
// The node's store is real, because what the heartbeat decides on is whether
// that store has a replicated estate open.
func TestALostEstateIsReopenedAtOnceAndTheFleetAskedOnItsInterval(t *testing.T) {
	t.Parallel()
	type harness struct {
		s *stateLog
		// asks is how many times the fleet was asked, and closedAtAsk how
		// many of those found no replicated estate open.
		asks, closedAtAsk atomic.Int64
	}
	boot := func(t *testing.T, ask func(h *harness) error) *harness {
		t.Helper()
		db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
			store.Options{}, estateLogs())
		t.Cleanup(func() { _ = db.Close() })
		ctx, cancel := context.WithCancel(t.Context())
		// THE NODE'S STORE IS WHAT A HEARTBEAT ASKS whether the replicated
		// estate is open, so the harness's state log runs over this one.
		h := &harness{s: &stateLog{nodeID: "node-0", db: db,
			run: ctx, stop: cancel}}
		t.Cleanup(h.s.Stop)
		h.s.rejoin = func(context.Context) error {
			h.asks.Add(1)
			if !replicatedOpen(h.s.db) {
				h.closedAtAsk.Add(1)
			}
			return ask(h)
		}
		return h
	}
	// request is one heartbeat that finds the node below the floor, and
	// reports whether it started anything.
	request := func(t *testing.T, h *harness) bool {
		t.Helper()
		h.s.requestRejoin(time.Now())
		h.s.rejoinMu.Lock()
		started := h.s.rejoining
		h.s.rejoinMu.Unlock()
		waitUntil(t, 10*time.Second, "the heartbeat's request to finish", func() bool {
			h.s.rejoinMu.Lock()
			defer h.s.rejoinMu.Unlock()
			return !h.s.rejoining
		})
		return started
	}
	pause := func(h *harness) (time.Duration, time.Time) {
		h.s.rejoinMu.Lock()
		defer h.s.rejoinMu.Unlock()
		return h.s.rejoinPause, h.s.rejoinAfter
	}
	widened := func(t *testing.T, h *harness, why string) {
		t.Helper()
		if p, after := pause(h); p != statelog.PositionHeartbeat || !after.After(time.Now()) {
			t.Fatalf("%s left a pause of %s until %s, want %s from now — the next "+
				"heartbeat asks the fleet again", why, p, after, statelog.PositionHeartbeat)
		}
	}

	t.Run("an ask that found no donor", func(t *testing.T) {
		t.Parallel()
		h := boot(t, func(*harness) error { return errNoDonor })
		request(t, h)
		widened(t, h, "no donor")
		if request(t, h) || h.asks.Load() != 1 {
			t.Fatalf("the next heartbeat asked the fleet again (%d asks) inside "+
				"the pause", h.asks.Load())
		}
	})

	t.Run("an ask that lost the estate", func(t *testing.T) {
		t.Parallel()
		h := boot(t, func(h *harness) error {
			if err := h.s.db.CloseReplicated(); err != nil {
				return err
			}
			return fmt.Errorf("%w: the artefact is installed and opening it "+
				"failed", statelog.ErrEstateNotRestored)
		})
		request(t, h)
		widened(t, h, "an ask that fetched and then lost the estate")
		if replicatedOpen(h.s.db) {
			t.Fatal("the staging did not leave the estate closed")
		}

		// THE NEXT HEARTBEAT, inside the pause: it reopens, and asks nobody.
		if !request(t, h) {
			t.Fatal("the next heartbeat did nothing — a node with no replicated " +
				"estate serves nothing, and reopening it waits on nobody")
		}
		if n := h.asks.Load(); n != 1 {
			t.Fatalf("the heartbeat that reopened the estate asked the fleet too "+
				"(%d asks) — a whole transfer again, inside the interval that "+
				"bounds exactly that", n)
		}
		if !replicatedOpen(h.s.db) {
			t.Fatal("the next heartbeat left the replicated estate closed")
		}
		widened(t, h, "the reopen")
	})

	t.Run("a reopen that fails", func(t *testing.T) {
		t.Parallel()
		h := boot(t, func(*harness) error { return errNoDonor })
		// A DIRECTORY WHERE THE FILE WAS: the store cannot open it, for a
		// reason an operator fixes, and it is fixed below by putting the
		// file back.
		path := h.s.db.ReplicatedFile()
		if err := h.s.db.CloseReplicated(); err != nil {
			t.Fatalf("close the replicated estate: %v", err)
		}
		if err := os.Rename(path, path+".aside"); err != nil {
			t.Fatalf("move the file aside: %v", err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("put a directory in its place: %v", err)
		}

		request(t, h)
		if replicatedOpen(h.s.db) {
			t.Fatal("the staging did not make the reopen fail")
		}
		if n := h.asks.Load(); n != 0 {
			t.Fatalf("the fleet was asked %d time(s) with no replicated estate "+
				"open — every step of a join reads it", n)
		}
		if p, after := pause(h); p != 0 || !after.IsZero() {
			t.Fatalf("a failed reopen set a pause of %s until %s — it asked "+
				"nobody, so it has nothing to wait out", p, after)
		}

		if err := os.Remove(path); err != nil {
			t.Fatalf("remove the directory: %v", err)
		}
		if err := os.Rename(path+".aside", path); err != nil {
			t.Fatalf("put the file back: %v", err)
		}
		request(t, h)
		if !replicatedOpen(h.s.db) {
			t.Fatal("the heartbeat after the cause was gone left the estate closed")
		}
		if h.asks.Load() != 1 || h.closedAtAsk.Load() != 0 {
			t.Fatalf("the fleet was asked %d time(s), %d of them over a closed "+
				"estate — want once, after the reopen", h.asks.Load(),
				h.closedAtAsk.Load())
		}
	})
}

// A RECREATION VERDICT ITS ROWS NO LONGER BEAR OUT IS RE-KEYED BY A JOIN.
//
// The join re-keys every runner to the file it leaves, against the live
// instant it read; the restore of an estate a failed join left closed reads the
// instant itself and cannot always. A runner whose re-key did not happen, over
// rows keyed to a stream it has never read, judges them by the one instant it
// has — and stops as recreated over rows that are the fleet's history, where a
// reanchor refuses because the rows ARE keyed to the live stream. The heartbeat
// reads the live instant every beat, so it is what names the verdict as stale
// and asks for the join that re-keys it.
//
// The verdict is staged as a failed re-key leaves it: recorded against an
// instant that is not the live stream's, over rows that are keyed to the live
// stream.
func TestARecreationVerdictItsRowsNoLongerBearOutIsReKeyedByAJoin(t *testing.T) {
	t.Parallel()
	e, _, _ := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	s := e.core.Load().log
	running := s.Domain(worklet.Domain{}.Name())
	name := running.domain.Name()
	if res, err := e.native.Load().writer.EvictNode(t.Context(), "op-before", "node-x"); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write for the rows to hold: %+v, %v", res, err)
	}
	stats, err := running.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the tracker's log: %v", err)
	}
	live := stats.CreatedAt

	s.haltApplier(name)
	at, keyed, _, err := statelog.CursorFor(t.Context(), s.db.Replicated(),
		running.spec.Name)
	if err != nil {
		t.Fatalf("read the checkpoint: %v", err)
	}
	never := live.Add(-time.Hour)
	if err := running.runner.Rejoined(at, keyed, never); err != nil {
		t.Fatalf("stage the verdict: %v", err)
	}
	s.resumeApplier(t.Context(), name)
	waitUntil(t, 10*time.Second, "the tracker to stop as recreated", func() bool {
		return errors.Is(running.runner.Stopped(), statelog.ErrStreamRecreated)
	})

	s.publishPositions(t.Context())
	waitUntil(t, 30*time.Second, "the heartbeat's join to re-key the tracker", func() bool {
		return running.runner.StreamIdentity() == nil && running.runner.Stopped() == nil
	})
	if got := running.runner.StreamCreatedAt(); statelog.IdentityOf(live, got, true) != statelog.StreamSame {
		t.Fatalf("the tracker is keyed to %s after the join, want the live %s", got, live)
	}
	if res, err := e.native.Load().writer.EvictNode(t.Context(), "op-after", "node-y"); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write after the join: %+v, %v", res, err)
	}
}

// A RESTORE WAITS FOR A RECOVERY IN PROGRESS.
//
// The restore re-keys every runner to the checkpoint the file it reopens keeps,
// and relaunches every applier. A reanchor between its reopen and that re-key
// would have its new checkpoint re-keyed back to the one the restore read
// before it, and its halted applier relaunched in the middle of its transition
// — so the restore takes the same exclusion the join and the reanchor take.
func TestARestoreWaitsForARecoveryInProgress(t *testing.T) {
	t.Parallel()
	e, back, _ := bootRejoinNode(t)
	s := e.core.Load().log
	quietHeartbeat(s)
	s.haltAppliers()
	if err := back.Store.CloseReplicated(); err != nil {
		t.Fatalf("close the replicated estate: %v", err)
	}

	// A RECOVERY IN PROGRESS holds the restore off, since the file the
	// restore reopens holds every log's rows.
	s.recovering.Lock()
	unlock := s.recovering.Unlock
	done := make(chan error, 1)
	go func() { done <- s.restoreEstate(s.run) }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("the restore ran to its end (%v) while a recovery held the node", err)
	case <-time.After(300 * time.Millisecond):
	}
	if replicatedOpen(back.Store) {
		unlock()
		t.Fatal("the restore reopened the estate while a recovery held the node")
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatalf("the restore, once the recovery ended: %v", err)
	}
	if !replicatedOpen(back.Store) {
		t.Fatal("the restore left the estate closed")
	}
}

// hydrated is [Engine.StateLogHydrated] as the condition a wait polls, under
// the test's own context.
func hydrated(t testing.TB, e *Engine) func() bool {
	t.Helper()
	return func() bool { return e.StateLogHydrated(t.Context()) }
}
