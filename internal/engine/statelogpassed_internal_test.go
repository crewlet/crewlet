package engine

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A NODE A PEER RE-ANCHORED PAST IS SENT TO ADOPT, BY THE JOIN AND BY THE
// HEARTBEAT ALIKE — and only on a domain whose nodes must agree on their rows.
//
// A reanchor opens its generation from ONE node's rows, so every other node
// left in the generation before holds a history the log no longer continues
// from, and nothing on the log can bring it level. Neither of the two things
// that sent a node to adopt could see it: its checkpoint is not below the log's
// first record, and it has one, so the fleet's generation was never asked. The
// only way out was deleting its database.
func TestANodeAPeerReanchoredPastIsSentToAdopt(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.native.log
	if res, err := e.native.writer.EvictNode(t.Context(), "op-1", "node-x"); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write: %+v, %v", res, err)
	}
	logs := map[string]*jetstream.DomainLog{}
	for _, name := range s.order {
		logs[name] = s.Domain(name).log
	}
	trackerName, vectorsName := tracker.Domain{}.Name(), search.Domain{}.Name()
	running := s.Domain(trackerName)
	own := running.runner.Committed()
	// A VECTORS CHECKPOINT, which a node with no embeddings configured has
	// never written: without one a node at generation 0 in a fleet past it is
	// behind by the no-checkpoint rule, whatever this case is about.
	vectors := s.Domain(vectorsName)
	vstats, err := vectors.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the vectors' log: %v", err)
	}
	if err := e.backends.Store.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor (stream, generation, seq, stream_created_at, updated_at)
			VALUES (?, 0, ?, ?, ?)
			ON CONFLICT (stream) DO NOTHING`,
			vectors.domain.Stream().Name, int64(vstats.LastSeq),
			store.EncodeTime(vstats.CreatedAt), store.EncodeTime(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("seed the vectors' checkpoint: %v", err)
	}

	// THE CONTROL: a fleet nobody re-anchored sends nobody to adopt.
	if _, passed, _, err := s.replayable(t.Context(), logs); err != nil || len(passed) != 0 {
		t.Fatalf("replayable = %v, %v on a fleet nobody re-anchored", passed, err)
	}

	// THE HEARTBEAT IS WATCHED RATHER THAN OBEYED, so what is read below is
	// its own conclusion rather than an adoption racing the assertion.
	var rejoins atomic.Int64
	s.rejoinMu.Lock()
	s.rejoin = func(context.Context) error { rejoins.Add(1); return nil }
	s.rejoinMu.Unlock()

	// A PEER RE-ANCHORED BOTH THE TRACKER'S LOG AND THE VECTORS'.
	if err := e.backends.Fleet.PutPositions(t.Context(), coord.NodePositions{
		NodeID: "reanchored-peer", At: time.Now().UTC(),
		Domains: map[string]coord.DomainPosition{
			trackerName: {Generation: own.Generation + 1, Seq: 1, AppliedThrough: 1},
			vectorsName: {Generation: 1, Seq: 1, AppliedThrough: 1},
		},
	}); err != nil {
		t.Fatalf("publish the peer's row: %v", err)
	}
	behind, passed, want, err := s.replayable(t.Context(), logs)
	if err != nil {
		t.Fatalf("replayable: %v", err)
	}
	if !slices.Equal(passed, []string{trackerName}) || len(behind) != 0 {
		t.Fatalf("replayable judged passed %v and behind %v, want the tracker passed "+
			"and nothing behind — its rows are the history before the peer's "+
			"reanchor, and the vectors re-anchor node by node", passed, behind)
	}
	if got := want.Generations[trackerName]; got != own.Generation+1 {
		t.Fatalf("the join would ask for the tracker at generation %d, want the "+
			"fleet's %d — every donor refuses an artefact from another generation",
			got, own.Generation+1)
	}
	if got := want.Generations[vectorsName]; got != s.Domain(vectorsName).runner.Committed().Generation {
		t.Fatalf("the join would ask for the vectors at generation %d, want this "+
			"node's own — a peer ahead there is not a history this node lost", got)
	}
	stats, err := running.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the tracker's log: %v", err)
	}
	if got := want.StreamCreatedAt[trackerName]; !got.Equal(stats.CreatedAt) {
		t.Fatalf("the join would ask against %s, want the live stream's %s",
			got, stats.CreatedAt)
	}

	// AND THE HEARTBEAT ASKS THE SAME QUESTION: it refuses the domain at once
	// and requests the rejoin that answers it.
	s.publishPositions(t.Context())
	if err := running.runner.StreamIdentity(); !errors.Is(err, statelog.ErrGenerationPassed) {
		t.Fatalf("after the heartbeat the tracker's identity is %v, want the passed "+
			"generation — it went on serving rows the fleet has left", err)
	}
	if err := s.Domain(vectorsName).runner.StreamIdentity(); err != nil {
		t.Fatalf("the vectors refuse over a peer that re-anchored its own copy: %v", err)
	}
	waitUntil(t, 5*time.Second, "the heartbeat to request a rejoin", func() bool {
		return rejoins.Load() > 0
	})
	if _, code := e.native.log.Established(t.Context(), true); code != statelog.RefuseWrongStream {
		t.Fatalf("a strict read on the tracker is answered %q, want %q",
			code, statelog.RefuseWrongStream)
	}
}

// A NODE LEFT ON A REBUILT LOG ADOPTS A RE-ANCHORED PEER'S SNAPSHOT AND
// FOLLOWS THE NEW GENERATION, WITH NO RESTART.
//
// The whole path, on a real broker: the log is rebuilt, a peer re-anchors it
// and publishes its position, and this node — stopped on the recreated log,
// refused by the guard that names that peer — is told by its heartbeat, adopts
// the peer's snapshot through the ordinary join, and its runner is re-keyed to
// the checkpoint the adoption installed. Before this the runner kept its
// verdict through the adoption and stopped again on the very checkpoint the
// join had installed.
func TestANodeLeftOnARebuiltLogAdoptsTheReanchoredGeneration(t *testing.T) {
	t.Parallel()
	e, back, q := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)
	s := e.native.log
	running := s.Domain(tracker.Domain{}.Name())
	if res, err := e.native.writer.EvictNode(t.Context(), "op-before", "node-x"); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write before the rebuild: %+v, %v", res, err)
	}
	rows := copyEstate(t, back)

	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rebuildLog(t, js, tracker.Domain{}.Stream())
	stats, err := running.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the rebuilt log: %v", err)
	}
	live := stats.CreatedAt.UTC()

	// THE PEER RE-ANCHORED IT: its checkpoint one below the rebuilt log's
	// first record, in the next generation, keyed to the live stream — and
	// it has written since.
	peerRecord := appendEviction(t, running, "op-peer", "node-z")
	next := running.runner.Committed().Generation + 1
	standUpDonor(t, q, rows, statelog.Position{
		Stream: running.domain.Stream().Name, Generation: next, Seq: peerRecord - 1,
	}, live)
	if err := e.backends.Fleet.PutPositions(t.Context(), coord.NodePositions{
		NodeID: "donor", At: time.Now().UTC(),
		Domains: map[string]coord.DomainPosition{running.domain.Name(): {
			Generation: next, Seq: peerRecord - 1, AppliedThrough: peerRecord - 1,
			StreamCreatedAt: live,
		}},
	}); err != nil {
		t.Fatalf("publish the peer's row: %v", err)
	}

	// THE GUARD NAMES THE PEER, and adopting its snapshot is what it means.
	if _, err := e.Reanchor(t.Context(), ReanchorRequest{
		Stream: running.domain.Stream().Name, Confirm: statelog.ConfirmationOf(live),
		By: "ops-1",
	}); !errors.Is(err, statelog.ErrReanchorRefused) {
		t.Fatalf("a reanchor with a re-anchored peer = %v, want a refusal", err)
	}

	s.publishPositions(t.Context())
	waitUntil(t, 90*time.Second, "the node to adopt the re-anchored generation", func() bool {
		at := running.runner.Committed()
		return running.runner.StreamIdentity() == nil && running.runner.Stopped() == nil &&
			at.Generation == next && at.Seq >= peerRecord
	})
	if n := countRows(t, e, `SELECT COUNT(*) FROM tracker_evictions
		WHERE node_id = 'node-z'`); n != 1 {
		t.Fatalf("the peer's record on the new generation applied %d time(s), want once", n)
	}
	// THE NODE ESTATE'S OWN RECORD, which is where an adoption is written:
	// the replicated file is the thing the adoption replaced.
	var completed int
	if err := back.Store.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM statelog_adoption
			WHERE completed_at IS NOT NULL`).Scan(&completed)
	}); err != nil || completed == 0 {
		t.Fatalf("the adoption rows say %d completed adoption(s) (%v), want one",
			completed, err)
	}
	if got := running.runner.StreamCreatedAt(); statelog.IdentityOf(live, got, true) != statelog.StreamSame {
		t.Fatalf("the runner is keyed to %s, want the rebuilt stream's %s", got, live)
	}
	waitUntil(t, 30*time.Second, "the node to admit seats again", e.NativeHydrated)
	res, err := e.native.writer.EvictNode(t.Context(), "op-after", "node-y")
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Position.Generation != next {
		t.Fatalf("a write after the adoption: %+v, %v — want it applied in generation %d",
			res, err, next)
	}
}

// A NODE A RESTORED-BROKER REANCHOR LEFT BEHIND STOPS ON THE PEER'S RECORD AND
// ADOPTS.
//
// The case neither the instant nor the log's end can show: a broker restored
// from an older copy keeps its stream, and a node whose checkpoint was below the
// restored end is not ahead of anything — its log looks like its own history,
// and the re-anchoring peer's generation record arrives as the next record. It
// is that record that stops it, before anything of the new generation lands on
// rows missing what the peer held past the copy; the heartbeat then sends it to
// adopt.
func TestANodeARestoredReanchorLeftBehindStopsOnItsRecordAndAdopts(t *testing.T) {
	t.Parallel()
	e, back, q := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)
	s := e.native.log
	running := s.Domain(tracker.Domain{}.Name())
	if res, err := e.native.writer.EvictNode(t.Context(), "op-before", "node-x"); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write: %+v, %v", res, err)
	}
	rows := copyEstate(t, back)
	stats, err := running.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	live := stats.CreatedAt.UTC()
	end := running.runner.Committed().Seq
	next := running.runner.Committed().Generation + 1

	// THE PEER'S REANCHOR, the restored case: its checkpoint at the log's end
	// in the next generation, and its generation record right after it.
	record, keeps, err := tracker.GenerationRecord{}.GenerationRecord(statelog.GenerationFacts{
		Generation: next, Case: statelog.ReanchorRestored, By: "ops-1", Writer: "donor",
		At: time.Now().UTC(),
		Inputs: statelog.ReanchorInputs{
			Stream: running.domain.Stream().Name, StreamCreatedAt: live, KeyedTo: live,
		},
	})
	if err != nil || !keeps {
		t.Fatalf("encode the peer's generation record: %v", err)
	}
	zero := uint64(0)
	spec := running.domain.Stream()
	genSeq, _, err := running.log.Append(t.Context(),
		spec.SubjectPrefix+"."+record.Subject.String(), record.OpID, &zero, record.Payload)
	if err != nil {
		t.Fatalf("append the peer's generation record: %v", err)
	}

	// THE RECORD STOPS THIS NODE before it is applied, and refuses the domain.
	waitUntil(t, 10*time.Second, "the tracker to stop on the peer's record", func() bool {
		return errors.Is(running.runner.StreamIdentity(), statelog.ErrGenerationPassed) &&
			running.runner.Stopped() != nil
	})
	if got := running.runner.Committed(); got.Seq != end {
		t.Fatalf("the tracker stands at %s, want sequence %d — the new generation's "+
			"record was applied into rows from the one before", got, end)
	}

	standUpDonor(t, q, rows, statelog.Position{
		Stream: spec.Name, Generation: next, Seq: end,
	}, live)
	if err := e.backends.Fleet.PutPositions(t.Context(), coord.NodePositions{
		NodeID: "donor", At: time.Now().UTC(),
		Domains: map[string]coord.DomainPosition{running.domain.Name(): {
			Generation: next, Seq: end, AppliedThrough: end, StreamCreatedAt: live,
		}},
	}); err != nil {
		t.Fatalf("publish the peer's row: %v", err)
	}
	s.publishPositions(t.Context())
	waitUntil(t, 90*time.Second, "the node to adopt the re-anchored generation", func() bool {
		at := running.runner.Committed()
		return running.runner.StreamIdentity() == nil && running.runner.Stopped() == nil &&
			at.Generation == next && at.Seq >= genSeq
	})
	if n := countRows(t, e, `SELECT COUNT(*) FROM tracker_log_generations
		WHERE generation = ?`, next); n != 1 {
		t.Fatalf("the peer's generation record applied %d time(s) after the "+
			"adoption, want once", n)
	}
}

// copyEstate copies a node's replicated database, which is what a peer that
// had applied exactly the same records holds.
func copyEstate(t *testing.T, back *Backends) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crewlet-replicated.db")
	if _, err := back.Store.Replicated().Backup(t.Context(), path); err != nil {
		t.Fatalf("copy the replicated estate: %v", err)
	}
	return path
}

// standUpDonor serves a snapshot of rows — with one domain's checkpoint moved to
// at, keyed to created — on the node's broker, the way a peer that re-anchored
// that domain would, until the test ends.
func standUpDonor(t *testing.T, q *jetstream.Queue, rows string,
	at statelog.Position, created time.Time) {

	t.Helper()
	copyDB, err := store.OpenEstate(t.Context(), store.EstateReplicated, rows, store.Options{})
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	if err := copyDB.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (stream) DO UPDATE SET
				generation = excluded.generation, seq = excluded.seq,
				stream_created_at = excluded.stream_created_at`,
			at.Stream, int64(at.Generation), int64(at.Seq),
			store.EncodeTime(created), store.EncodeTime(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("move the copy's checkpoint: %v", err)
	}
	if err := copyDB.Close(); err != nil {
		t.Fatalf("close the copy: %v", err)
	}
	if err := store.QuiesceCopy(t.Context(), rows); err != nil {
		t.Fatalf("quiesce the copy: %v", err)
	}
	dir := filepath.Dir(rows)
	donorNode, err := store.Open(t.Context(), filepath.Join(dir, "node.db"),
		store.Options{ReplicatedPath: rows})
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
	snapDir := filepath.Join(dir, "snapshots")
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
		Path:   func(m statelog.Manifest) string { return filepath.Join(snapDir, m.Artifact) },
	})
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	ctx, stop := context.WithCancel(t.Context())
	t.Cleanup(stop)
	go func() { _ = donor.Serve(ctx) }()
}
