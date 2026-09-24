package engine

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A NODE BELOW THE FLEET'S GENERATION TAKES THE FLEET'S GENERATION, AND ADOPTS
// — WHETHER IT HOLDS NO CHECKPOINT OR ONE AT THE GENERATION THE FLEET LEFT.
//
// In a fleet that has re-anchored, the records before the re-anchor are not on
// the log. A node that replayed from the new stream's sequence 1 would stamp
// every row at its own generation while its peers hold the same records at N,
// report itself caught up over a database holding only what was published
// since, and block the fleet's trim, whose applied term reads a counted node
// at a lower generation as unknown. And the floor comparison made at its own
// generation against a floor published at N fails the boot on the call that
// exists to send it to adopt — so the decision below is either an adoption or
// an error, and an error here is a node that can never come up.
//
// Mutation: take a found checkpoint's own generation rather than the fleet's
// and the first case fails its boot on the floor comparison; take the node's
// own zero generation for an absent checkpoint rather than the fleet's and the
// second fails the same way; ask for the checkpoint's own generation while
// comparing at the fleet's and both ask for an artefact no peer holds.
func TestANodeBelowTheFleetsGenerationTakesItAndAdopts(t *testing.T) {
	t.Parallel()
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
	s := e.native.log
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)

	// THE TRIM IS QUIESCED FIRST, because it is the other writer of the
	// floor this case forges. `PutFloor` replaces a domain's floor
	// wholesale, and this node's own retention loop publishes one per
	// domain per tick at the generation its rows are on — zero — so a tick
	// landing after the write below erases the fleet's generation 3 and the
	// case asserts against a floor it did not publish. `stopRetention`
	// waits for an in-flight tick, so after it returns nothing else writes.
	e.stopRetention()

	name := tracker.Domain{}.Name()
	stream := tracker.Domain{}.Stream().Name

	// THE FLEET HAS RE-ANCHORED: an operator moved it to generation 3, and
	// the trim has published a floor there.
	if err := back.Fleet.PutFloor(t.Context(), coord.TrimFloor{
		Domain: name, Generation: 3, TrimTo: 0,
		BlockedBy: "applied", At: time.Now().UTC(), By: "peer",
	}); err != nil {
		t.Fatalf("publish a floor at the fleet's generation: %v", err)
	}

	logs := map[string]*jetstream.DomainLog{}
	for _, domain := range registeredDomains() {
		running := s.Domain(domain.Name())
		if running == nil {
			t.Fatalf("%s is not running", domain.Name())
		}
		logs[domain.Name()] = running.log
	}
	decides := func(t *testing.T, what string) {
		t.Helper()
		behind, want, err := s.replayable(t.Context(), logs)
		if err != nil {
			t.Fatalf("replayable with %s: %v — the node compared its own "+
				"generation against the fleet's floor and failed its boot on the "+
				"call that would have sent it to adopt", what, err)
		}
		if got := want.Generations[name]; got != 3 {
			t.Errorf("with %s the node would ask for a generation-%d artefact, "+
				"want 3 — every donor refuses an artefact from another "+
				"generation, so a request below it is one no peer can satisfy",
				what, got)
		}
		if !slices.Contains(behind, name) {
			t.Errorf("with %s the node judged itself able to replay %s in a "+
				"fleet at generation 3 — the records before the re-anchor are "+
				"not on the log", what, name)
		}
	}

	// THIS NODE HOLDS A CHECKPOINT AT THE GENERATION THE FLEET LEFT — a
	// peer of the node that re-anchored, restarted to follow it. Written
	// here, because a company that has published nothing to this log has
	// no checkpoint on it yet.
	const left = 1
	running := s.Domain(name)
	if err := s.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor (stream, generation, seq, stream_created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (stream) DO UPDATE SET
				generation = excluded.generation, seq = excluded.seq`,
			stream, left, 0, store.EncodeTime(running.identity()),
			store.EncodeTime(time.Now()))
		return err
	}); err != nil {
		t.Fatalf("write a checkpoint at generation %d: %v", left, err)
	}
	decides(t, fmt.Sprintf("a checkpoint at generation %d", left))

	// AND THIS NODE HOLDS NO CHECKPOINT AT ALL — a machine added to the
	// company, or one whose replicated estate was lost and rebuilt.
	if err := s.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`DELETE FROM statelog_cursor WHERE stream = ?`, stream)
		return err
	}); err != nil {
		t.Fatalf("clear the checkpoint: %v", err)
	}
	decides(t, "no checkpoint")
}

// A NODE THAT ADOPTS ONTO A LOG REBUILT UNDER IT FOLLOWS THE LIVE STREAM.
//
// A log deleted and rebuilt under a running node is named by the heartbeat,
// which latches the recreation, and the node's reads refuse. A peer that
// followed the rebuild holds rows checkpointed against the new stream, and
// once this node falls below that stream's floor it adopts them. What it must
// not keep is the stream its appliers BOOTED against: the relaunched applier
// would compare the adopted checkpoint with it and stop as though its log had
// been recreated, and the latch would keep every read refusing, until a
// restart read the live stream again.
//
// Mutation: drop the rejoin's re-identification and the relaunched applier
// stops on the adopted checkpoint; keep the latch and the node refuses
// `wrong_stream` after adopting.
func TestANodeThatAdoptsOntoARebuiltLogFollowsTheLiveStream(t *testing.T) {
	t.Parallel()
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
	s := e.native.log
	running := s.Domain(tracker.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	spec := tracker.Domain{}.Stream()
	log := running.log
	waitUntil(t, 20*time.Second, "the node to catch up on its own log", func() bool {
		_, end, err := log.Bounds(t.Context())
		return err == nil && running.runner.Committed().Seq == end
	})
	at := running.runner.Committed()
	booted := running.identity()

	// THE REBUILD, with this node's appliers ended first, so nothing
	// applies the new stream's records into rows keyed by the old one
	// before the adoption this case is about.
	s.haltAppliers()
	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteStream(t.Context(), spec.Name); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	// The broker keeps creation instants at nanoseconds and a checkpoint
	// keeps microseconds; a rebuild inside one microsecond would read as
	// the same stream.
	time.Sleep(2 * time.Millisecond)
	if _, err := js.CreateStream(t.Context(), natsjs.StreamConfig{
		Name: spec.Name, Subjects: spec.Subjects,
		MaxBytes: 16 << 20, Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("rebuild the stream: %v", err)
	}
	stats, err := log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the rebuilt stream: %v", err)
	}
	live := stats.CreatedAt.UTC()
	if statelog.IdentityOf(booted, live, true) != statelog.StreamRecreated {
		t.Fatalf("the rebuilt stream reports %s and the node booted against %s, "+
			"so this case is not the shape it names", live, booted)
	}

	// RECORDS PAST THIS NODE'S CHECKPOINT ON THE NEW STREAM, purged, so the
	// node is below that stream's floor and its heartbeat asks the fleet
	// for a snapshot.
	barrier, err := tracker.EncodeBarrier(statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind}, Gen: at.Generation,
		Scope: statelog.ScopeSet{Paths: []string{statelog.BarrierScope}},
	})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	appendBarrier := func() uint64 {
		t.Helper()
		seq, _, err := log.Append(t.Context(), spec.SubjectPrefix+"."+statelog.BarrierKind,
			"", nil, barrier)
		if err != nil {
			t.Fatalf("append a barrier: %v", err)
		}
		return seq
	}
	var last uint64
	for range at.Seq + 2 {
		last = appendBarrier()
	}
	if err := log.Purge(t.Context(), last); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// THE DONOR: this node's rows checkpointed at `last` on the rebuilt
	// stream, which is what a peer that followed the rebuild holds — a
	// barrier writes no rows.
	serveSnapshot(t, back, q, map[string]statelog.Position{
		spec.Name: {Stream: spec.Name, Generation: at.Generation, Seq: last},
	}, map[string]time.Time{spec.Name: live})

	waitUntil(t, 90*time.Second, "the node to adopt the donor's snapshot", func() bool {
		return running.runner.Committed().Seq == last
	})
	// ITS APPLIER RUNS ON THE REBUILT STREAM: the next record there applies.
	next := appendBarrier()
	waitUntil(t, 30*time.Second, "the relaunched applier to apply the rebuilt "+
		"log's next record", func() bool {
		return running.runner.Stopped() != nil || running.runner.Committed().Seq >= next
	})
	if err := running.runner.Stopped(); err != nil {
		t.Fatalf("the applier stopped after adopting a checkpoint on the live "+
			"stream: %v", err)
	}
	if got := running.identity(); !got.Equal(live) {
		t.Errorf("the domain runs against a stream created at %s, want the live "+
			"one's %s", got, live)
	}
	health, err := s.health(t.Context(), running)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.StreamRecreated {
		t.Error("the recreation latched before the adoption still stands, so " +
			"every read on a node that follows the live stream refuses wrong_stream")
	}
	// AND THE HEARTBEAT COMPARES AGAINST THE STREAM IT FOLLOWS NOW.
	s.publishPositions(t.Context())
	if running.recreated.Load() {
		t.Error("the heartbeat named the stream this node follows as recreated")
	}
}

// serveSnapshot stands up a donor on the node's own broker, serving a snapshot
// of the node's replicated rows with the named streams' checkpoints moved to
// the given positions and stream identities — what a peer holding those rows
// at those positions would donate.
func serveSnapshot(t *testing.T, back *Backends, q *jetstream.Queue,
	at map[string]statelog.Position, created map[string]time.Time) {

	t.Helper()
	donorDir := t.TempDir()
	replicated := filepath.Join(donorDir, "crewlet-replicated.db")
	if _, err := back.Store.Replicated().Backup(t.Context(), replicated); err != nil {
		t.Fatalf("copy the replicated estate: %v", err)
	}
	copyDB, err := store.OpenEstate(t.Context(), store.EstateReplicated, replicated, store.Options{})
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	for stream, pos := range at {
		if err := copyDB.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `
				INSERT INTO statelog_cursor
					(stream, generation, seq, stream_created_at, updated_at)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (stream) DO UPDATE SET
					generation = excluded.generation, seq = excluded.seq,
					stream_created_at = excluded.stream_created_at`,
				stream, int64(pos.Generation), int64(pos.Seq),
				store.EncodeTime(created[stream]), store.EncodeTime(time.Now().UTC()))
			return err
		}); err != nil {
			t.Fatalf("move the copy's checkpoint on %s: %v", stream, err)
		}
	}
	if err := copyDB.Close(); err != nil {
		t.Fatalf("close the copy: %v", err)
	}
	if err := store.QuiesceCopy(t.Context(), replicated); err != nil {
		t.Fatalf("quiesce the copy: %v", err)
	}
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
			Health: func() statelog.Health { return statelog.Health{CaughtUp: true, Lag: &lag} },
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
	donor, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: "donor",
		Dial:   func(context.Context) (*nats.Conn, error) { return q.DialOwned() },
		Newest: func() (statelog.Manifest, bool) { return manifest, true },
		Path: func(m statelog.Manifest) string {
			return filepath.Join(snapDir, m.Artifact)
		},
	})
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	donorCtx, stopDonor := context.WithCancel(t.Context())
	t.Cleanup(stopDonor)
	go func() { _ = donor.Serve(donorCtx) }()
}
