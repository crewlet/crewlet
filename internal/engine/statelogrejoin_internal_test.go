package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
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
// barrier writes no rows. Then the heartbeat is left to notice.
func TestANodeBelowTheFloorAdoptsWhileRunning(t *testing.T) {
	t.Parallel()
	e, back, q := bootRejoinNode(t)
	running, at, last := pushBelowTheFloor(t, e, q)

	// THE DONOR: this node's own rows at the purged position, which is
	// what a peer that applied those barriers holds.
	donorDir := t.TempDir()
	replicated := filepath.Join(donorDir, "crewlet-replicated.db")
	copyAdvancedTo(t, back, running, at, last, replicated)
	donorNode, err := store.Open(t.Context(), filepath.Join(donorDir, "node.db"),
		store.Options{ReplicatedPath: replicated})
	if err != nil {
		t.Fatalf("open the donor's store: %v", err)
	}
	t.Cleanup(func() { _ = donorNode.Close() })
	lag := uint64(0)
	var registered []statelog.Registered
	for _, domain := range registeredDomains() {
		registered = append(registered, statelog.Registered{
			Domain: domain,
			Health: func() statelog.Health { return statelog.Health{Drained: true, Lag: &lag} },
		})
	}
	snapDir := filepath.Join(donorDir, "snapshots")
	snapper, err := statelog.NewSnapshotter(statelog.SnapshotDeps{
		Domains: registered, DB: donorNode, Dir: snapDir, NodeID: "donor",
		EngineVersion: "v0.0.0-test",
		Counted:       func(context.Context) (int, error) { return 2, nil },
		Interval:      24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	manifest, err := snapper.Take(t.Context())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	// answered is when the donor first answered with its artefact: the
	// latest instant a donor's snapshotter could have finished the file it
	// offers, so the latest one the adoption's bound may precede.
	var answered atomic.Int64
	donor, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: "donor",
		Dial:   func(context.Context) (*nats.Conn, error) { return q.DialOwned() },
		Newest: func() (statelog.Manifest, bool) {
			answered.CompareAndSwap(0, time.Now().UnixNano())
			return manifest, true
		},
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
	donorCtx, stopDonor := context.WithCancel(t.Context())
	t.Cleanup(stopDonor)
	go func() { _ = donor.Serve(donorCtx) }()

	// THE HEARTBEAT NOTICES, the node adopts, and its appliers come back
	// over the artefact — with no restart and no operator.
	waitUntil(t, 90*time.Second, "the node to adopt the donor's snapshot", func() bool {
		return running.runner.Committed().Seq == last
	})
	// THE ADOPTION IS RECORDED AS COMPLETE, in this node's own estate and
	// naming this donor's artefact.
	var recorded string
	var startedAt int64
	var completedAt sql.NullInt64
	if err := back.Store.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `
			SELECT manifest, started_at, completed_at FROM statelog_adoption
			ORDER BY started_at DESC LIMIT 1`).Scan(&recorded, &startedAt, &completedAt)
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
	// AND ITS START FOLLOWS THE DONOR'S ANSWER, which is what makes it the
	// watermark an artefact from a donor that scrubbed its ledger is
	// installed with: the artefact may have been finished an instant before
	// the donor answered, and an operation this node published before THAT
	// can sit inside it with its ledger row scrubbed. A start stamped when
	// the join began precedes the ask itself.
	if began, asked := store.DecodeTime(startedAt),
		time.Unix(0, answered.Load()).UTC().Truncate(time.Microsecond); began.Before(asked) {
		t.Fatalf("the adoption row starts at %s, before the donor answered at %s — "+
			"an operation minted between the two can be inside the artefact with "+
			"its ledger row scrubbed, and an adoption that stopped before "+
			"completing would let it be re-decided", began, asked)
	}
	// AND THE LEDGER LOST NOTHING: a donor of this build scrubs none, so the
	// adopter holds its donor's rows and inherits its donor's watermark —
	// which says nothing lost, on a donor that never swept. An adopter that
	// recorded a loss here would answer its own backlog `unknown`.
	rows, err := tracker.NewRows(back.Store)
	if err != nil {
		t.Fatalf("build the tracker's read seam: %v", err)
	}
	if before, lost, err := rows.LostBefore(t.Context()); err != nil || lost {
		t.Fatalf("the adopted ledger may have lost rows before %s (%v, %v) — a "+
			"ledger that travelled lost nothing, and every first attempt minted "+
			"before the join would be refused", before, lost, err)
	}
	waitUntil(t, 30*time.Second, "the node to admit seats again", e.NativeHydrated)
	if ok, domain := e.SeatsServiceable(); !ok {
		t.Fatalf("the node cannot keep its seats after adopting: %s", domain)
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
// after it. Silent is also what a fleet whose donors hold nothing looks like —
// this node's own donor, holding no snapshot, is one — so the window is spent
// exactly as it would be in production.
func TestAStopMidRejoinEndsTheJoinAndWaitsForItsAppliers(t *testing.T) {
	t.Parallel()
	e, _, q := bootRejoinNode(t)
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
	s := e.native.log
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
func bootRejoinNode(t *testing.T) (*Engine, *Backends, *jetstream.Queue) {
	t.Helper()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	q, ok := back.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend", back.Queue)
	}
	return e, back, q
}

// pushBelowTheFloor leaves a running node below its tracker log's floor, which
// is exactly what the fleet's trim does to a node that has been away: its
// appliers stop, two barriers land on the log that it never applies, and the
// log is purged past them. It returns the tracker's running domain, the
// position the node had applied, and the last sequence purged — which is the
// position a peer that applied those barriers would hold.
func pushBelowTheFloor(t *testing.T, e *Engine, q *jetstream.Queue) (
	running *runningDomain, at statelog.Position, last uint64) {

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
	running *runningDomain, at statelog.Position, log *jetstream.DomainLog, last uint64) {

	t.Helper()
	spec := tracker.Domain{}.Stream()
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	s := e.native.log
	running = s.Domain(tracker.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}

	// Settle: whatever the boot wrote to the tracker's log is applied.
	waitUntil(t, 20*time.Second, "the node to catch up on its own log", func() bool {
		_, end, err := log.Bounds(t.Context())
		return err == nil && running.runner.Committed().Seq == end
	})
	at = running.runner.Committed()

	s.haltAppliers()
	body, err := tracker.EncodeBarrier(statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind}, Gen: at.Generation,
		Scope: statelog.ScopeSet{Paths: []string{statelog.BarrierScope}},
	})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	for range 2 {
		if last, _, err = log.Append(t.Context(), spec.SubjectPrefix+"."+statelog.BarrierKind, "", nil, body); err != nil {
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
func copyAdvancedTo(t *testing.T, back *Backends, running *runningDomain,
	at statelog.Position, last uint64, path string) {

	t.Helper()
	if _, err := back.Store.Replicated().Backup(t.Context(), path); err != nil {
		t.Fatalf("copy the replicated estate: %v", err)
	}
	copyDB, err := store.OpenEstate(t.Context(), store.EstateReplicated, path, store.Options{})
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	if err := copyDB.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (stream) DO UPDATE SET
				seq = excluded.seq, stream_created_at = excluded.stream_created_at`,
			tracker.Domain{}.Stream().Name, int64(at.Generation), int64(last),
			store.EncodeTime(running.runner.StreamCreatedAt()), store.EncodeTime(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("advance the copy's checkpoint: %v", err)
	}
	if err := copyDB.Close(); err != nil {
		t.Fatalf("close the copy: %v", err)
	}
	if err := store.QuiesceCopy(t.Context(), path); err != nil {
		t.Fatalf("quiesce the copy: %v", err)
	}
}

func waitUntil(t *testing.T, within time.Duration, what string, cond func() bool) {
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
			s := e.native.log
			quietHeartbeat(s)
			if c.below {
				pushBelowTheFloor(t, e, q)
			} else {
				s.haltAppliers()
			}
			if err := back.Store.CloseReplicated(); err != nil {
				t.Fatalf("close the replicated estate: %v", err)
			}

			if err := s.restoreEstate(s.run); err != nil {
				t.Fatalf("restore: %v", err)
			}
			if back.Store.Replicated() == nil {
				t.Fatal("the restore left the replicated estate closed — nothing " +
					"else in a running node reopens it")
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
		})
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
			if err := back.Store.ReopenReplicated(t.Context()); err != nil {
				t.Fatalf("open the installed artefact: %v", err)
			}
			return e.rejoin(s.run, s)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e, back, q := bootRejoinNode(t)
			s := e.native.log
			quietHeartbeat(s)
			running, at, _, last := appendPastTheNode(t, e, q)
			artefact := filepath.Join(t.TempDir(), "artefact.db")
			copyAdvancedTo(t, back, running, at, last, artefact)
			// WHAT A JOIN LEAVES when it installed an artefact and could
			// not open it: the file renamed into place, the estate closed.
			if err := back.Store.CloseReplicated(); err != nil {
				t.Fatalf("close the replicated estate: %v", err)
			}
			if err := store.AdoptFile(t.Context(), back.Store.ReplicatedPath(), artefact); err != nil {
				t.Fatalf("install the artefact: %v", err)
			}

			if err := c.open(t, e, back, s); err != nil {
				t.Fatalf("%s over the installed artefact: %v", c.name, err)
			}
			js, err := natsjs.New(q.Conn())
			if err != nil {
				t.Fatalf("open a JetStream handle: %v", err)
			}
			cons, err := js.Consumer(t.Context(), tracker.Domain{}.Stream().Name,
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
		db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
		if err != nil {
			t.Fatalf("open the store: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		ctx, cancel := context.WithCancel(t.Context())
		h := &harness{s: &stateLog{nodeID: "node-0", db: db, run: ctx, stop: cancel}}
		t.Cleanup(h.s.Stop)
		h.s.rejoin = func(context.Context) error {
			h.asks.Add(1)
			if h.s.db.Replicated() == nil {
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
		if p, after := pause(h); p != PositionHeartbeat || !after.After(time.Now()) {
			t.Fatalf("%s left a pause of %s until %s, want %s from now — the next "+
				"heartbeat asks the fleet again", why, p, after, PositionHeartbeat)
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
		if h.s.db.Replicated() != nil {
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
		if h.s.db.Replicated() == nil {
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
		path := h.s.db.ReplicatedPath()
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
		if h.s.db.Replicated() != nil {
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
		if h.s.db.Replicated() == nil {
			t.Fatal("the heartbeat after the cause was gone left the estate closed")
		}
		if h.asks.Load() != 1 || h.closedAtAsk.Load() != 0 {
			t.Fatalf("the fleet was asked %d time(s), %d of them over a closed "+
				"estate — want once, after the reopen", h.asks.Load(),
				h.closedAtAsk.Load())
		}
	})
}
