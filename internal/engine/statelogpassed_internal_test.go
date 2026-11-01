package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"sync"
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
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
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
	s := e.core.Load().log
	if res, err := e.native.Load().writer.EvictNode(t.Context(), "op-1", "node-x"); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write: %+v, %v", res, err)
	}
	logs := map[string]*jetstream.DomainLog{}
	for _, name := range s.held().order {
		logs[name] = s.Domain(name).log
	}
	trackerName, vectorsName := worklet.Domain{}.Name(), search.Domain{}.Name()
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
			INSERT INTO statelog_cursor (stream, generation, seq, stream_created_at, updated_at, applied_version)
			VALUES (?, 0, ?, ?, ?, 0)
			ON CONFLICT (stream) DO NOTHING`,
			vectors.spec.Name, int64(vstats.LastSeq),
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
	if _, code := e.core.Load().log.Established(t.Context(), true); code != statelog.RefuseWrongStream {
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
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	// NOTHING BUT THIS CASE APPENDS to the tracker's log from here: a pin of
	// the collector's landing on the rebuilt log ahead of the peer's record
	// would be the record the donor's checkpoint sits on.
	quietTheCollector(t, e)
	s := e.core.Load().log
	running := s.Domain(worklet.Domain{}.Name())
	if res, err := e.native.Load().writer.EvictNode(t.Context(), "op-before", "node-x"); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write before the rebuild: %+v, %v", res, err)
	}
	rows := copyEstate(t, back)

	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rebuildLog(t, js, worklet.Domain{}.Stream())
	stats, err := running.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the rebuilt log: %v", err)
	}
	live := stats.CreatedAt.UTC()

	// THE PEER RE-ANCHORED IT: its checkpoint one below the rebuilt log's
	// first record, in the next generation, keyed to the live stream — and
	// it has written since.
	peerRecord := appendEviction(t, running, "op-peer", "node-z")
	if peerRecord != 1 {
		t.Fatalf("the peer's record landed at %d on the rebuilt log, want its first "+
			"— something else appended to it, and the donor's checkpoint below the "+
			"record would sit on that", peerRecord)
	}
	next := running.runner.Committed().Generation + 1
	standUpDonor(t, e, q, rows, statelog.Position{
		Stream: running.spec.Name, Generation: next, Seq: peerRecord - 1,
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
		Stream: running.spec.Name, Confirm: statelog.ConfirmationOf(live),
		By: reanchorOperator,
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
	waitUntil(t, 30*time.Second, "the node to admit seats again", hydrated(t, e))
	res, err := e.native.Load().writer.EvictNode(t.Context(), "op-after", "node-y")
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
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	// NOTHING BUT THIS CASE APPENDS to the tracker's log from here, because
	// the case is a log whose end is the copy's checkpoint with the peer's
	// generation record right after it.
	quietTheCollector(t, e)
	s := e.core.Load().log
	running := s.Domain(worklet.Domain{}.Name())
	res, err := e.native.Load().writer.EvictNode(t.Context(), "op-before", "node-x")
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write: %+v, %v", res, err)
	}
	stance := running.runner.Stance()
	rows := copyEstate(t, back)
	// THE RUNNER'S STANCE ACROSS THE COPY is what tells apart the two ways the
	// copy can miss the write below: a stance that moved is an estate rejoined
	// or reloaded under the case, and one that did not is a copy read at a
	// view older than the write — which store.DB.Backup holds a view of its
	// own to rule out.
	moved := running.runner.Stance() != stance
	// THE DONOR STANDS WHERE THE COPY DOES — on the copy's own checkpoint row,
	// never the live runner's read after the copy, which may already have
	// moved past the rows copied: the donor's artefact would then name that
	// later sequence beside the record the copy's checkpoint names, and the
	// adoption stops on the mismatch as a diverged log. And the copy holds
	// the write: a copy taken once the write answered applied has its rows.
	copied := copiedCheckpoint(t, rows, running.spec.Name)
	if copied.Seq != res.Position.Seq {
		t.Fatalf("the copy's checkpoint is at %d, want the write's own %d — a copy "+
			"taken after a write answered applied did not hold it (the runner's "+
			"stance moved across the copy: %t)", copied.Seq, res.Position.Seq, moved)
	}
	stats, err := running.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	live := stats.CreatedAt.UTC()
	end := copied.Seq
	next := copied.Generation + 1

	// THE PEER'S REANCHOR, the restored case: its checkpoint at the log's end
	// in the next generation, and its generation record right after it.
	record, keeps, err := worklet.GenerationRecord{}.GenerationRecord(statelog.GenerationFacts{
		Generation: next, Case: statelog.ReanchorRestored, By: "ops-1", Writer: "donor",
		At: time.Now().UTC(),
		Inputs: statelog.ReanchorInputs{
			Stream: running.spec.Name, StreamCreatedAt: live, KeyedTo: live,
		},
	})
	if err != nil || !keeps {
		t.Fatalf("encode the peer's generation record: %v", err)
	}
	zero := uint64(0)
	spec := running.spec
	genSeq, _, err := running.log.Append(t.Context(),
		spec.SubjectPrefix+"."+record.Subject.String(), record.OpID, &zero,
		running.signer.Seal(record.Payload))
	if err != nil {
		t.Fatalf("append the peer's generation record: %v", err)
	}
	if genSeq != end+1 {
		t.Fatalf("the peer's generation record landed at %d, want %d right after "+
			"the copy's checkpoint — something else appended to the tracker's log "+
			"during the case", genSeq, end+1)
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

	standUpDonor(t, e, q, rows, statelog.Position{
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
	if _, err := storetest.ReplicatedDB(t, back.Store.Replicated()).Backup(t.Context(), path); err != nil {
		t.Fatalf("copy the replicated estate: %v", err)
	}
	return path
}

// copiedCheckpoint is the checkpoint a copy of the estate holds for stream —
// the position its rows were applied through, which is where a donor serving
// that copy stands.
func copiedCheckpoint(t *testing.T, rows, stream string) statelog.Position {
	t.Helper()
	copyDB, err := store.OpenEstate(t.Context(), store.EstateReplicated, rows, store.Options{})
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	defer func() { _ = copyDB.Close() }()
	var gen, seq int64
	if err := copyDB.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT generation, seq FROM statelog_cursor
			WHERE stream = ?`, stream).Scan(&gen, &seq)
	}); err != nil {
		t.Fatalf("read the copy's checkpoint: %v", err)
	}
	return statelog.Position{Stream: stream, Generation: uint32(gen), Seq: uint64(seq)}
}

// standUpDonor serves a snapshot of rows — with one domain's checkpoint moved to
// at, keyed to created — on the node's broker, the way a peer that re-anchored
// that domain would, until the test ends.
//
// AS A LIVE DATA NODE OF e's FLEET, which is what every donor is: a join waits
// for the live data nodes to answer and no longer, so a donor nobody's
// presence names is one a join may stop before hearing ([presentAsDataNode]).
//
// IT RETURNS ONCE THE DONOR ANSWERS AN ASK, and not merely once it has been
// started: see [awaitDonor].
func standUpDonor(t *testing.T, e *Engine, q *jetstream.Queue, rows string,
	at statelog.Position, created time.Time) {

	t.Helper()
	copyDB, err := store.OpenEstate(t.Context(), store.EstateReplicated, rows, store.Options{})
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	log, err := q.DomainLog(t.Context(), at.Stream)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	moveCheckpoint(t, log, copyDB, at, created)
	if err := copyDB.Close(); err != nil {
		t.Fatalf("close the copy: %v", err)
	}
	if err := store.QuiesceCopy(t.Context(), rows); err != nil {
		t.Fatalf("quiesce the copy: %v", err)
	}
	dir := filepath.Dir(rows)
	donorNode, _ := storetest.OpenEstate(t, filepath.Join(dir, "node.db"), store.Options{ReplicatedPath: rows}, 1)
	t.Cleanup(func() { _ = donorNode.Close() })
	lag := uint64(0)
	var registered []statelog.Registered
	for _, domain := range registeredDomains() {
		registered = append(registered, statelog.Registered{
			Domain: domain, Spec: domain.Stream(),
			Health: func() statelog.Health { return statelog.Health{Drained: true, Lag: &lag} },
		})
	}
	snapDir := filepath.Join(dir, "snapshots")
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
	serveDonor(t, e, q.Conn(), statelog.DonorDeps{
		NodeID: "donor",
		Dial:   func(context.Context) (*nats.Conn, error) { return q.DialOwned() },
		Newest: func() (statelog.Manifest, bool) { return manifest, true },
		Path:   func(m statelog.Manifest) string { return filepath.Join(snapDir, m.Artifact) },
	})
}

// serveDonor serves deps as a donor until the test ends, and returns once it
// answers an offer request on nc's broker: see [awaitDonor]. Presented as a
// live data node of e's fleet first ([presentAsDataNode]), where there is an
// engine to join from; nil stands a donor up on the broker alone.
func serveDonor(t *testing.T, e *Engine, nc *nats.Conn, deps statelog.DonorDeps) {
	t.Helper()
	// up is closed once the donor's own connection is open, which is where
	// awaitDonor's clock starts. Once, because how often Serve dials is that
	// package's business and a second close would panic.
	up := make(chan struct{})
	opened := sync.OnceFunc(func() { close(up) })
	if dial := deps.Dial; dial != nil {
		deps.Dial = func(ctx context.Context) (*nats.Conn, error) {
			conn, err := dial(ctx)
			if err == nil {
				opened()
			}
			return conn, err
		}
	}
	donor, err := statelog.NewDonor(deps)
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	if e != nil {
		presentAsDataNode(t, e, deps.NodeID)
	}
	ctx, stop := context.WithCancel(t.Context())
	// CLOSED after the send, so the cleanup's receive returns even when
	// awaitDonor has already taken the error.
	served := make(chan error, 1)
	go func() { served <- donor.Serve(ctx); close(served) }()
	t.Cleanup(func() { stop(); <-served })
	awaitDonor(t, nc, deps.NodeID, up, served)
}

// A DONOR IS AWAITED ON ITS DIAL'S OWN BUDGET, and only its answer on the offer
// window.
//
// [awaitDonor] once bounded its whole wait by one [statelog.OfferWindow] from
// the moment Serve was started, so a donor whose handshake ran longer than that
// — which the embedded broker's listener budget exists to allow on a loaded host
// — failed every case standing it up, as a donor no join would hear, while
// every join asked once its connection was open would have been answered. This
// donor's dial takes a second longer than the window and has to be awaited.
func TestADonorWithASlowDialIsAwaitedOnItsDialsOwnBudget(t *testing.T) {
	t.Parallel()
	q, err := jetstream.Open(t.Context(), jetstream.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open the broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	slow := statelog.OfferWindow + time.Second
	serveDonor(t, nil, q.Conn(), statelog.DonorDeps{
		NodeID: "slow",
		Dial: func(ctx context.Context) (*nats.Conn, error) {
			select {
			case <-time.After(slow):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return q.DialOwned()
		},
		Newest: func() (statelog.Manifest, bool) { return statelog.Manifest{NodeID: "slow"}, true },
		Path:   func(statelog.Manifest) string { return "" },
	})
}

// awaitDonor returns once the donor named node answers an offer request on
// nc's broker — the condition every caller of [standUpDonor] depends on.
//
// # Why started is not serving
//
// [statelog.Donor.Serve] dials its own connection and subscribes from its own
// goroutine, and a joiner asks ONCE: [statelog.CollectOffers] publishes one
// request and collects for a window, so a donor whose subscription the broker
// registers after that publish never hears it, however long the window runs.
// A case that stood a donor up and then called [Engine.rejoin] directly ran
// exactly that race. Measured, the donor dials within milliseconds of being
// started and the rejoin asks fifty milliseconds to a second after, so the
// case passed whenever the donor won; one that loses it fails the case as `no
// peer could donate a usable snapshot`, which a donor dialling four hundred
// milliseconds late reproduces exactly. The heartbeat-driven callers asked
// again a heartbeat later and only ran slow.
//
// # Why this is the condition
//
// A donor's own answer is the only thing that proves the broker holds its
// subscription: an answer to an ask published on the same subject means every
// later ask reaches it too. Asking the subject rather than any responder,
// because this node's own donor listens on it as well and answers first. The
// ask is REPEATED, since one published before the subscription landed is lost
// rather than delivered late, and every answer is read from one inbox, so an
// answer arriving after the next ask went out still counts.
//
// # The bounds, one per half
//
// THE CONNECTION is waited for on the dial's own budget, and on no clock of
// this function's: up is closed once the donor's dial hands back a connection
// ([serveDonor]), and a dial that fails or runs out its handshake timeout ends
// Serve, which served reports by name. For [jetstream.Queue.DialOwned], which
// every donor here dials through, that timeout is the embedded broker's accept
// budget — thirty seconds, because nats's own two seconds failed boots on a
// loaded host against a server that was up — so a clock here would be a
// second, shorter opinion about the same handshake. It was one: the whole wait
// was once bounded by a single offer window, and a donor whose dial took six
// seconds, well inside the budget it is actually given, failed this helper
// while every join asked after it would have been answered.
//
// THE ANSWER is waited for one [statelog.OfferWindow] from the moment the
// connection is open: subscribing and answering is the same local work a
// connected donor does for a joiner's ask, and that window is what the
// framework gives a donor to do it. One that cannot fails the case here,
// naming the donor, rather than in the adoption with an error that names
// nothing.
func awaitDonor(t *testing.T, nc *nats.Conn, node string, up <-chan struct{},
	served <-chan error) {

	t.Helper()
	select {
	case <-up:
	case err := <-served:
		t.Fatalf("donor %q stopped before its connection was open: %v", node, err)
	}
	inbox := nats.NewInbox()
	answers, err := nc.SubscribeSync(inbox)
	if err != nil {
		t.Fatalf("open the probe's inbox: %v", err)
	}
	defer func() { _ = answers.Unsubscribe() }()
	ask, err := json.Marshal(statelog.OfferRequest{NodeID: "probe"})
	if err != nil {
		t.Fatalf("encode the probe's ask: %v", err)
	}
	const between = 20 * time.Millisecond
	deadline := time.Now().Add(statelog.OfferWindow)
	for time.Now().Before(deadline) {
		select {
		case err := <-served:
			t.Fatalf("donor %q stopped serving before it answered: %v", node, err)
		default:
		}
		if err := nc.PublishRequest(statelog.SubjectOffer, inbox, ask); err != nil {
			t.Fatalf("ask donor %q: %v", node, err)
		}
		for {
			msg, err := answers.NextMsg(between)
			if errors.Is(err, nats.ErrTimeout) {
				break
			}
			// NOBODY SUBSCRIBES AT ALL, which the broker says at once:
			// wait out the interval rather than asking at its reply rate.
			if errors.Is(err, nats.ErrNoResponders) {
				time.Sleep(between)
				break
			}
			if err != nil {
				t.Fatalf("read donor %q's answer: %v", node, err)
			}
			var offer statelog.Offer
			if json.Unmarshal(msg.Data, &offer) == nil && offer.Manifest.NodeID == node {
				return
			}
		}
	}
	t.Fatalf("donor %q did not answer an offer request within %s of its "+
		"connection opening, which is the window a join would have given it",
		node, statelog.OfferWindow)
}

// presentAsDataNode claims a presence lease for id as a data node on e's
// fleet, so a join e makes lists it and expects its answer
// ([statelog.AdoptDeps.Donors]).
func presentAsDataNode(t testing.TB, e *Engine, id string) {
	t.Helper()
	claimPresences(t, e.backends.Coord, map[string][]string{id: {"data"}})
}
