package engine

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

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
	spec := tracker.Domain{}.Stream()
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	s := e.native.log
	running := s.Domain(tracker.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}

	// Settle: whatever the boot wrote to the tracker's log is applied.
	waitUntil(t, 20*time.Second, "the node to catch up on its own log", func() bool {
		_, end, err := log.Bounds(t.Context())
		return err == nil && running.runner.Committed().Seq == end
	})
	at := running.runner.Committed()

	// THE NODE STOPS APPLYING, two barriers land that it never sees, and
	// the log is purged past them.
	s.haltAppliers()
	body, err := tracker.EncodeBarrier(statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind}, Gen: at.Generation,
		Scope: statelog.ScopeSet{Paths: []string{statelog.BarrierScope}},
	})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	var last uint64
	for range 2 {
		if last, _, err = log.Append(t.Context(), spec.SubjectPrefix+"."+statelog.BarrierKind, "", nil, body); err != nil {
			t.Fatalf("append a barrier: %v", err)
		}
	}
	if last != at.Seq+2 {
		t.Fatalf("the barriers landed at %d, want %d", last, at.Seq+2)
	}
	if err := log.Purge(t.Context(), last); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// THE DONOR: this node's own rows at the purged position, which is
	// what a peer that applied those barriers holds.
	donorDir := t.TempDir()
	replicated := filepath.Join(donorDir, "crewlet-replicated.db")
	if _, err := back.Store.Replicated().Backup(t.Context(), replicated); err != nil {
		t.Fatalf("copy the replicated estate: %v", err)
	}
	copyDB, err := store.OpenEstate(t.Context(), store.EstateReplicated, replicated, store.Options{})
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
			spec.Name, int64(at.Generation), int64(last),
			store.EncodeTime(running.createdAt), store.EncodeTime(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("advance the copy's checkpoint: %v", err)
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
			Domain:          domain,
			Health:          func() statelog.Health { return statelog.Health{Drained: true, Lag: &lag} },
			StreamCreatedAt: running.createdAt,
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
	if _, adopted, err := statelog.AdoptedAt(t.Context(), back.Store); err != nil || !adopted {
		t.Fatalf("the adoption row says (%v, %v), want a completed adoption", adopted, err)
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
