package statelog_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// ---- the applier's fakes -------------------------------------------- //
//
// The domain arrives eleven steps later. What is under test is the framework's
// loop, which is decided entirely by what these answer — so a fake that can be
// driven into each branch is a stronger test than a real domain that only
// reaches the easy ones.

// probeApplier is a state machine whose whole job is to be observable: it
// writes one row per record into a table the test can count.
type probeApplier struct {
	mu       sync.Mutex
	applied  []statelog.Position
	commits  int
	rows     int
	gate     statelog.Reason
	gated    map[uint64]bool
	failAt   uint64
	rowsPer  int
	slowFrom uint64
	slowFor  time.Duration
	now      func() time.Time
}

func newProbeApplier() *probeApplier {
	return &probeApplier{gated: map[uint64]bool{}, rowsPer: 1}
}

func (a *probeApplier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record, opts statelog.ApplyOptions) (int, error) {
	a.mu.Lock()
	fail, per, slowFrom, slowFor := a.failAt, a.rowsPer, a.slowFrom, a.slowFor
	a.mu.Unlock()
	if fail != 0 && rec.Position.Seq == fail {
		return 0, fmt.Errorf("the probe applier refuses sequence %d", fail)
	}
	if slowFrom != 0 && rec.Position.Seq >= slowFrom && a.now != nil {
		// The clock is injected, so "slow" is deterministic rather than
		// a sleep the scheduler decides the length of.
		a.mu.Lock()
		a.rows += 0
		a.mu.Unlock()
		_ = slowFor
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO probe_rows (position, kind, stored_at) VALUES (?, ?, ?)
		 ON CONFLICT (position) DO NOTHING`,
		rec.Position.Packed(), rec.Kind, store.EncodeTime(opts.StoredAt)); err != nil {
		return 0, err
	}
	a.mu.Lock()
	a.applied = append(a.applied, rec.Position)
	a.mu.Unlock()
	return per, nil
}

func (a *probeApplier) Gated(_ context.Context, _ *sql.Tx, rec statelog.Record) (statelog.Reason, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gate, a.gated[rec.Position.Seq], nil
}

func (a *probeApplier) Committed(context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commits++
}

func (a *probeApplier) seen() []statelog.Position {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]statelog.Position(nil), a.applied...)
}

// probeFetch hands the loop whatever the test queued, and records what was
// acknowledged.
type probeFetch struct {
	mu      sync.Mutex
	queue   []statelog.Message
	acked   map[uint64]int
	fetches int
}

func newProbeFetch() *probeFetch { return &probeFetch{acked: map[uint64]int{}} }

// offer queues one record at seq, encoded as the probe domain's envelope.
func (f *probeFetch) offer(seq uint64, env statelog.Envelope) {
	body, err := json.Marshal(env)
	if err != nil {
		panic(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, statelog.Message{
		Seq:      seq,
		StoredAt: time.Unix(1_700_000_000, 0).UTC().Add(time.Duration(seq) * time.Second),
		Payload:  body,
		Ack: func() error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.acked[seq]++
			return nil
		},
	})
}

// Fetch waits for the queue the way a real pull consumer waits for its
// stream: an empty broker blocks for the caller's own wait rather than
// answering nothing immediately, which would turn the apply loop into a spin.
func (f *probeFetch) Fetch(ctx context.Context, maxMessages, maxBytes int, wait time.Duration) ([]statelog.Message, error) {
	deadline := time.Now().Add(wait)
	for {
		f.mu.Lock()
		f.fetches++
		if len(f.queue) > 0 {
			n := min(len(f.queue), maxMessages)
			out := f.queue[:n]
			f.queue = f.queue[n:]
			f.mu.Unlock()
			return out, nil
		}
		f.mu.Unlock()
		if time.Now().After(deadline) {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (f *probeFetch) Pending(context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return uint64(len(f.queue)), nil
}

func (f *probeFetch) ackCount(seq uint64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acked[seq]
}

func (f *probeFetch) ackedAll() map[uint64]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uint64]int{}
	for k, v := range f.acked {
		out[k] = v
	}
	return out
}

// ---- the harness ----------------------------------------------------- //

type applyHarness struct {
	t       *testing.T
	db      *store.DB
	runner  *statelog.Runner
	applier *probeApplier
	fetch   *probeFetch
}

func newApplyHarness(t *testing.T, domain statelog.Domain) *applyHarness {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "node.db"), store.Options{
		PinnedWriters: 1,
	})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})

	// THE DOMAIN'S OWN TABLES, in plain DDL. A real domain ships them in
	// its own migration; the framework's migration deliberately creates
	// only its own three, so a second domain adds a file rather than
	// editing one that has already shipped.
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), probeDDL)
		return err
	}); err != nil {
		t.Fatalf("create the probe domain's tables: %v", err)
	}

	applier := newProbeApplier()
	fetch := newProbeFetch()
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain:     domain,
		Applier:    applier,
		Fetch:      fetch,
		DB:         db.Replicated(),
		Generation: 1,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return &applyHarness{t: t, db: db, runner: runner, applier: applier, fetch: fetch}
}

const probeDDL = `
CREATE TABLE probe_rows (
    position  INTEGER NOT NULL PRIMARY KEY,
    kind      TEXT    NOT NULL,
    stored_at INTEGER NOT NULL
);
CREATE TABLE probe_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL
);
CREATE INDEX probe_ops_swept_idx ON probe_ops (applied_at);
CREATE TABLE probe_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    payload      BLOB    NOT NULL,
    stored_at    INTEGER NOT NULL
);
CREATE INDEX probe_log_deferred_subject_idx
    ON probe_log_deferred (subject_id, subject_kind);
CREATE TABLE probe_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX probe_deferred_scope_path_idx ON probe_deferred_scope (path);
`

// run drives the loop until it has consumed everything queued, or fails.
func (h *applyHarness) run(want uint64) error {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), 20*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if h.runner.Committed().Seq >= want {
			cancel()
			<-errs
			return nil
		}
		if err := h.runner.Stopped(); err != nil {
			cancel()
			<-errs
			return err
		}
		select {
		case err := <-errs:
			return err
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	<-errs
	return fmt.Errorf("the applier reached %s, want sequence %d",
		h.runner.Committed(), want)
}

func env(seq uint64, kind, id, opID string, version int, scope ...string) statelog.Envelope {
	if len(scope) == 0 {
		scope = []string{"object/" + id}
	}
	return statelog.Envelope{
		V:       version,
		Kind:    kind,
		Subject: statelog.Subject{Kind: "object", ID: id},
		OpID:    opID,
		Gen:     1,
		Scope:   statelog.ScopeSet{Paths: scope},
		Writer:  "node-a",
	}
}

// ---- the cases ------------------------------------------------------- //

// THE ROWS, THE OPERATION ID, THE ANCHOR AND THE CHECKPOINT COMMIT TOGETHER,
// and the acknowledgement is outside that transaction.
//
// This is the one contract every consumer gets for free and can break
// invisibly. The nearest neighbour in this tree does the opposite for a
// correct reason — it commits its batch and then writes its cursor, because a
// crash between the two replays a batch it can always redeliver. That is false
// for a log that gets trimmed: the replay it counts on is a replay of records
// the trim has removed.
func TestTheCheckpointCommitsWithTheRowsItCovers(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	for seq := uint64(1); seq <= 5; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprintf("o%d", seq), fmt.Sprintf("op-%d", seq), 1))
	}
	if err := h.run(5); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Every record's row, its operation and its anchor are all present at
	// the checkpoint. Reading them in one transaction is the assertion:
	// the contract is that they are simultaneous, not that they exist.
	var rows, ops, anchors, cursor int64
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM probe_rows`).Scan(&rows); err != nil {
			return err
		}
		if err := tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM probe_ops`).Scan(&ops); err != nil {
			return err
		}
		if err := tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM statelog_anchor`).Scan(&anchors); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(), `SELECT seq FROM statelog_cursor`).Scan(&cursor)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if rows != 5 || ops != 5 || anchors != 5 || cursor != 5 {
		t.Fatalf("rows=%d ops=%d anchors=%d cursor=%d, want 5 of each — a "+
			"checkpoint ahead of the rows it covers is a node that replays "+
			"nothing and holds nothing", rows, ops, anchors, cursor)
	}
	// AND EVERY DELIVERY IS ACKNOWLEDGED EXACTLY ONCE.
	for seq := uint64(1); seq <= 5; seq++ {
		if got := h.fetch.ackCount(seq); got != 1 {
			t.Errorf("sequence %d was acknowledged %d time(s), want 1", seq, got)
		}
	}
}

// A RECORD THIS BUILD CANNOT DECODE IS RETAINED, and the checkpoint advances
// past it.
//
// Retained rather than skipped, because the bytes are the only copy: a rolling
// upgrade puts records on the wire the older half has never heard of, and
// dropping them would make every upgrade an outage. The checkpoint advances
// because the log CONSUMED the record — a checkpoint that stopped would
// redeliver it for ever and never apply anything above it.
func TestARecordThisBuildCannotDecodeIsRetainedAtItsPosition(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 9)) // above this build
	h.fetch.offer(3, env(3, "edit", "c", "op-3", 1))
	if err := h.run(3); err != nil {
		t.Fatalf("run: %v", err)
	}

	var deferredPos, scopeRows int64
	var version int64
	var payload []byte
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(),
			`SELECT position, version, payload FROM probe_log_deferred`).
			Scan(&deferredPos, &version, &payload); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM probe_deferred_scope`).Scan(&scopeRows)
	}); err != nil {
		t.Fatalf("read the deferred record: %v", err)
	}
	at := statelog.Position{Stream: probeStream, Generation: 1, Seq: 2}
	if deferredPos != at.Packed() {
		t.Errorf("the deferred record is at packed %d, want %d — it must be "+
			"replayed at its ORIGINAL position", deferredPos, at.Packed())
	}
	if version != 9 {
		t.Errorf("the deferred record records version %d, want 9 — that number "+
			"is what tells an operator which build to run", version)
	}
	if len(payload) == 0 {
		t.Error("the deferred record kept no payload — lossless means the bytes")
	}
	if scopeRows == 0 {
		t.Error("the deferred record's scope was not indexed — a deferral " +
			"recorded without its scope is a record that makes rows stale with " +
			"nothing able to see that it does")
	}

	// THE CHECKPOINT IS PAST IT and the record after it applied.
	if got := h.runner.Committed().Seq; got != 3 {
		t.Fatalf("the checkpoint is at %d, want 3", got)
	}
	if got := len(h.applier.seen()); got != 2 {
		t.Fatalf("the applier saw %d record(s), want 2 — the retained one must "+
			"not reach it", got)
	}
	// AND ITS DELIVERY IS ACKNOWLEDGED. An unacknowledged retained record
	// is redelivered for ever, and the checkpoint drops it every time.
	if got := h.fetch.ackCount(2); got != 1 {
		t.Fatalf("the retained record was acknowledged %d time(s), want 1", got)
	}
	if _, held := h.runner.Deferred(); !held {
		t.Error("the runner does not report holding a deferred record")
	}
}

// A LATER RECORD ON STALE ROWS IS RETAINED TOO, and it is the half a
// per-record decode check cannot see.
//
// Applying a record this build CAN read on top of rows a deferred record never
// wrote produces state no other node holds — and nothing later can tell that
// it did, because both nodes think they are current.
func TestARecordOverAStaleScopeIsRetainedRatherThanApplied(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	// A record this build cannot read, whose scope names the CONTAINER of
	// the object the next record edits. There is nothing on the next
	// record's own subject to say so.
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 9, "project/ENG"))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1, "project/ENG/object/b"))
	h.fetch.offer(3, env(3, "edit", "c", "op-3", 1, "project/OPS/object/c"))
	if err := h.run(3); err != nil {
		t.Fatalf("run: %v", err)
	}

	seen := h.applier.seen()
	if len(seen) != 1 || seen[0].Seq != 3 {
		t.Fatalf("the applier saw %v, want only sequence 3 — a record inside a "+
			"deferred scope must be retained, and one outside it must not be",
			seen)
	}
	var deferred int64
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM probe_log_deferred`).Scan(&deferred)
	}); err != nil {
		t.Fatalf("count the deferred records: %v", err)
	}
	if deferred != 2 {
		t.Fatalf("%d record(s) retained, want 2 — the second is the one whose "+
			"rows the first made stale", deferred)
	}
}

// A GATE DROPS A DURABLE RECORD, and the anchor and the checkpoint still move.
//
// A gate is a rule under which a durable record applies NOWHERE. Its position
// is still the subject's last message, so the anchor must record it — a writer
// forming its expectation from anything else re-reads the number that produced
// its own rejection until it runs out of rounds.
func TestAGatedRecordWritesNoRowsAndStillMovesTheAnchor(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.applier.gate = statelog.ReasonEvicted
	h.applier.gated[2] = true
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "a", "op-2", 1))
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}

	var rows, ops int64
	var anchor int64
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM probe_rows`).Scan(&rows); err != nil {
			return err
		}
		if err := tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM probe_ops`).Scan(&ops); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(),
			`SELECT anchor FROM statelog_anchor WHERE subject = ?`,
			probePrefix+".object.a").Scan(&anchor)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if rows != 1 || ops != 1 {
		t.Fatalf("rows=%d ops=%d, want 1 of each — a gated record writes neither",
			rows, ops)
	}
	want := statelog.Position{Stream: probeStream, Generation: 1, Seq: 2}
	if anchor != want.Packed() {
		t.Fatalf("the anchor is at packed %d, want %d — a gated record's "+
			"position is still the subject's last message, and an anchor that "+
			"stopped short wedges every later writer on that subject",
			anchor, want.Packed())
	}
}

// AN ANCHOR IS WRITTEN ONLY FOR AN ARBITRATED KIND.
//
// Arbitration is a property of the subject KIND rather than of the stream, and
// a stream-wide rule is not merely wasteful: the framework's own barrier
// shares ONE subject across every read in the company, so it would put an
// upsert per read on a single hot row inside the transaction that holds this
// store's only writer — and it would falsify the property that a barrier
// writes zero rows and never enters the applier's budget at all.
func TestAnAnchorIsWrittenOnlyForAnArbitratedKind(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	arbitrated := env(1, "edit", "a", "op-1", 1)
	additive := env(2, "turn", "a", "op-2", 1)
	additive.Subject = statelog.Subject{Kind: "turn", ID: "a"}
	h.fetch.offer(1, arbitrated)
	h.fetch.offer(2, additive)
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}

	var anchors int64
	var subject string
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM statelog_anchor`).Scan(&anchors); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(),
			`SELECT subject FROM statelog_anchor`).Scan(&subject)
	}); err != nil {
		t.Fatalf("read the anchors: %v", err)
	}
	if anchors != 1 {
		t.Fatalf("%d anchor row(s), want 1 — only the arbitrated kind gets one",
			anchors)
	}
	if !strings.HasSuffix(subject, ".object.a") {
		t.Fatalf("the anchor is on %q, want the arbitrated subject", subject)
	}
}

// AN APPLY GATE THIS BUILD CANNOT READ IS A STOP, and it is the one place the
// retain rule inverts.
//
// A deferred gate does not postpone one record's effect on one node — it
// silently LICENSES every record above it. An eviction deferred by the node it
// evicts leaves that node's own eviction rows empty, which is the one fence
// still fresh when its coordination path is wedged, so it passes every fence
// it has and publishes records every peer drops.
func TestARecordThatInstallsAGateStopsRatherThanDefers(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, gatingDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "eviction", "node-b", "op-2", 9)) // a gate, unreadable
	h.fetch.offer(3, env(3, "edit", "c", "op-3", 1))

	err := h.run(3)
	if err == nil {
		t.Fatal("the applier ran past a gate it cannot read")
	}
	if !errors.Is(err, statelog.ErrStopped) {
		t.Fatalf("error = %v, want ErrStopped", err)
	}
	for _, want := range []string{"eviction", "9"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the stop does not name %q: %v", want, err)
		}
	}
	if got := h.runner.Committed().Seq; got >= 2 {
		t.Fatalf("the checkpoint is at %d — a stop must not advance past the "+
			"record it stopped on", got)
	}
	var deferred int64
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM probe_log_deferred`).Scan(&deferred)
	}); err != nil {
		t.Fatalf("count the deferred records: %v", err)
	}
	if deferred != 0 {
		t.Fatalf("%d gate(s) retained — a gate is a stop, and retaining one "+
			"licenses every record above it", deferred)
	}
}

// gatingDomain answers that an eviction record installs an apply gate.
type gatingDomain struct{ probeDomain }

func (gatingDomain) InstallsGate(env statelog.Envelope) bool {
	return env.Kind == "eviction"
}

// A REDELIVERY IS ACKNOWLEDGED AND APPLIED ONCE.
//
// The checkpoint is what makes an apply idempotent at a position, and a
// duplicate arriving in the SAME run is the case the committed cursor cannot
// catch: it is above the cursor the transaction started from, and at or below
// something that transaction has already applied.
func TestARedeliveredRecordIsAppliedOnceAndAcknowledged(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1)) // the same record again
	h.fetch.offer(3, env(3, "edit", "c", "op-3", 1))
	if err := h.run(3); err != nil {
		t.Fatalf("run: %v", err)
	}

	seen := h.applier.seen()
	if len(seen) != 3 {
		t.Fatalf("the applier saw %d record(s), want 3 — a redelivery must not "+
			"be applied twice", len(seen))
	}
	var rows int64
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM probe_rows`).Scan(&rows)
	}); err != nil {
		t.Fatalf("count the rows: %v", err)
	}
	if rows != 3 {
		t.Fatalf("%d row(s), want 3", rows)
	}
	// EVERY DELIVERY IS ACKNOWLEDGED, including the duplicate: an
	// unacknowledged delivery is redelivered for ever.
	acked := h.fetch.ackedAll()
	if acked[2] != 2 {
		t.Fatalf("sequence 2 was delivered twice and acknowledged %d time(s) — "+
			"an acknowledgement is by IDENTITY, and a delivery nothing "+
			"acknowledges comes back for ever", acked[2])
	}
}

// A COMPACTED DOMAIN HAS NO CONTIGUITY TO TEST, and its deferred records key
// on the SUBJECT.
//
// Its stream keeps one message per subject, so an ordinary write removes an
// interior sequence — a strict loop would stall on the first one for ever, and
// a positional retention would keep records the stream itself has superseded.
func TestACompactedDomainStepsOverHolesAndSupersedesItsDeferrals(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, compactedDomain{})
	h.fetch.offer(10, env(10, "vector", "a", "op-10", 1))
	h.fetch.offer(40, env(40, "vector", "b", "op-40", 9)) // deferred
	h.fetch.offer(90, env(90, "vector", "b", "op-90", 9)) // supersedes it
	if err := h.run(90); err != nil {
		t.Fatalf("run: %v", err)
	}

	var count, position, orphans int64
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM probe_log_deferred`).Scan(&count); err != nil {
			return err
		}
		// THE SCOPE CHILD IS SUPERSEDED WITH ITS PARENT. There is no
		// foreign key, so the child is found through the parent's own
		// position — and a supersede that removed the parent first
		// would leave rows no record owns, which every later probe
		// reads as a deferral this node cannot clear: the read barrier
		// waits on it and the writer's step 0 refuses to publish.
		if err := tx.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM probe_deferred_scope s
			WHERE NOT EXISTS (
				SELECT 1 FROM probe_log_deferred d WHERE d.position = s.position)`).
			Scan(&orphans); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(),
			`SELECT position FROM probe_log_deferred`).Scan(&position)
	}); err != nil {
		t.Fatalf("read the deferred records: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("%d scope row(s) outlived the record they index — a probe "+
			"reads them as a deferral this node holds and cannot clear, so "+
			"the read barrier waits and the write path refuses, for ever",
			orphans)
	}
	if count != 1 {
		t.Fatalf("%d deferred record(s) for one subject, want 1 — a compacted "+
			"domain's stream holds one message per subject, so a positional "+
			"retention would keep records the stream has already superseded",
			count)
	}
	at := statelog.Position{Stream: probeStream, Generation: 1, Seq: 90}
	if position != at.Packed() {
		t.Fatalf("the retained record is at packed %d, want the newer %d",
			position, at.Packed())
	}
}

// compactedDomain is the other replay protocol.
type compactedDomain struct{ probeDomain }

func (compactedDomain) Stream() statelog.StreamSpec {
	s := probeDomain{}.Stream()
	s.Replay = statelog.ReplayCompacted
	s.MaxPerSubject = 1
	s.MaxAge = time.Hour
	// A COMPACTED DOMAIN DECLARES NO ARBITRATED KIND: it publishes no
	// per-subject expectation, and its idempotency is its own row guard.
	s.ArbitratedKinds = nil
	return s
}

// A WAITER IS WOKEN ONCE, BY THE COMMIT THAT SATISFIED IT.
//
// The shape this replaces broadcasts on every commit and wakes every waiter to
// re-test its own target, with a goroutine per wait to turn a deadline into a
// wake. At the rate this framework creates — a barrier per linearizable read,
// and an applier that yields its batch to serve it — that is every waiter woken
// per read.
func TestAWaiterIsWokenOnceByItsOwnCommit(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()

	target := statelog.Position{Stream: probeStream, Generation: 1, Seq: 3}
	woken := make(chan error, 1)
	go func() { woken <- h.runner.WaitCommitted(ctx, target) }()

	for seq := uint64(1); seq <= 4; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprintf("o%d", seq), fmt.Sprintf("op-%d", seq), 1))
	}
	select {
	case err := <-woken:
		if err != nil {
			t.Fatalf("the waiter was woken with %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the waiter was never woken")
	}
	if got := h.runner.Committed().Packed(); got < target.Packed() {
		t.Fatalf("the waiter was woken at %d, below its target %d",
			got, target.Packed())
	}
	cancel()
	<-errs

	// AND A STOPPED APPLIER RELEASES EVERYBODY. A waiter left blocked on
	// one waits out its whole budget for a position nothing will reach.
	if got := h.runner.Waiting(); got != 0 {
		t.Fatalf("%d waiter(s) left behind by a stopped applier", got)
	}
}

// THE ACKNOWLEDGEMENT IS BY IDENTITY AND THE TAIL IS CARRIED.
//
// Two defects wearing one shape. A run carries records that were applied,
// records retained because this build cannot read them, records a gate
// dropped and records already below the checkpoint — so acknowledging "as many
// as were applied" acknowledges the WRONG ONES: the retained record's delivery
// stays open and is redelivered for ever, and an applied record's is
// acknowledged in its place. And after a budget ends a transaction early, the
// rest of the run is neither acknowledged nor carried, so every
// budget-bounded transaction leaves a hole the loop waits out.
func TestAckIsByIdentityAndTheTailIsCarried(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	// Each record costs most of the row budget, so the transaction ends
	// after the second one — at a RECORD boundary, never inside a commit.
	h.applier.rowsPer = statelog.ApplyTxRowBudget/2 + 1
	h.applier.gate = statelog.ReasonDeleted
	h.applier.gated[3] = true

	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 9)) // retained
	h.fetch.offer(3, env(3, "edit", "c", "op-3", 1)) // gated
	h.fetch.offer(4, env(4, "edit", "d", "op-4", 1))
	h.fetch.offer(5, env(5, "edit", "e", "op-5", 1))
	if err := h.run(5); err != nil {
		t.Fatalf("run: %v", err)
	}

	// EVERY DELIVERY, whatever became of its record, exactly once.
	acked := h.fetch.ackedAll()
	for seq := uint64(1); seq <= 5; seq++ {
		if acked[seq] != 1 {
			t.Errorf("sequence %d was acknowledged %d time(s), want 1 — an "+
				"acknowledgement is by identity, and the retained and gated "+
				"records are the ones a count gets wrong", seq, acked[seq])
		}
	}
	if got := h.runner.Committed().Seq; got != 5 {
		t.Fatalf("the checkpoint is at %d, want 5 — the tail after a budget "+
			"break must be carried into the next transaction rather than "+
			"dropped", got)
	}
	// AND THE BUDGET ACTUALLY BOUND SOMETHING. A case that never reaches
	// the break is a case that asserts nothing about the tail.
	if got := len(h.applier.seen()); got != 3 {
		t.Fatalf("the applier saw %d record(s), want 3 — one retained and one "+
			"gated must not reach it", got)
	}
}

// A HOLE IN A STRICT LOG IS WAITED OUT, NOT STEPPED OVER.
//
// A log is contiguous by construction, so a hole is a redelivery in flight
// rather than the stream's own doing. Applying past it produces state no other
// node holds — and nothing later can tell that it did, because the checkpoint
// moved and both nodes think they are current.
func TestAStrictLoopWaitsForAHoleRatherThanApplyingPastIt(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(3, env(3, "edit", "c", "op-3", 1)) // above a hole at 2

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()

	// The loop reaches 1 and stops there: 3 is held above the hole.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.runner.Committed().Seq < 1 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := h.runner.Committed().Seq; got != 1 {
		cancel()
		<-errs
		t.Fatalf("the checkpoint is at %d, want 1 — a record above a hole must "+
			"not be applied", got)
	}
	// Steady state: it must NOT advance to 3 while 2 is missing.
	time.Sleep(100 * time.Millisecond)
	if got := h.runner.Committed().Seq; got != 1 {
		cancel()
		<-errs
		t.Fatalf("the checkpoint advanced to %d over a missing sequence", got)
	}

	// The missing record arrives, and both apply.
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && h.runner.Committed().Seq < 3 {
		time.Sleep(2 * time.Millisecond)
	}
	got := h.runner.Committed().Seq
	cancel()
	<-errs
	if got != 3 {
		t.Fatalf("the checkpoint is at %d after the hole closed, want 3", got)
	}

	seen := h.applier.seen()
	if len(seen) != 3 {
		t.Fatalf("the applier saw %d record(s), want 3", len(seen))
	}
	for i := range seen {
		if seen[i].Seq != uint64(i+1) {
			t.Fatalf("the applier saw %v — a strict log applies in strictly "+
				"increasing order and nothing else may reorder it", seen)
		}
	}
}

// AN APPLIER'S FAILURE ON ONE RECORD DOES NOT MOVE THE CHECKPOINT.
//
// The transaction rolls back, so the rows, the anchor, the operation id and
// the checkpoint all go back together — which is the whole point of committing
// them together, and the property a node "can only be behind, never
// inconsistent" rests on.
func TestAFailedApplyLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.applier.failAt = 2
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := h.runner.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "refuses sequence 2") {
		t.Fatalf("Run = %v, want the applier's own refusal", err)
	}
	if got := h.runner.Committed().Seq; got != 0 {
		t.Fatalf("the checkpoint is at %d after a rolled-back transaction, want "+
			"0 — the record before the failure was in the same transaction", got)
	}
	var rows, anchors int64
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM probe_rows`).Scan(&rows); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM statelog_anchor`).Scan(&anchors)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if rows != 0 || anchors != 0 {
		t.Fatalf("rows=%d anchors=%d after a rolled-back transaction, want 0 — "+
			"a node can be behind and must never be inconsistent", rows, anchors)
	}
	// NOTHING IS ACKNOWLEDGED either: the acknowledgement is outside the
	// transaction and after it, so a rollback leaves the deliveries open
	// for a redelivery this node can apply.
	if len(h.fetch.ackedAll()) != 0 {
		t.Fatalf("%d delivery(ies) acknowledged for a transaction that rolled "+
			"back", len(h.fetch.ackedAll()))
	}
}

// APPLY IS STRICTLY BY SEQUENCE ACROSS TRANSACTION BOUNDARIES.
//
// The budget splits a batch at a RECORD boundary and the next transaction
// resumes at the next sequence with no re-sorting — which is what makes "two
// nodes at one checkpoint hold the same rows" a statement about the log's
// order rather than about how either node happened to batch it.
func TestApplyIsStrictlyBySequenceAcrossTransactionBoundaries(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	// Each record costs a third of the row budget, so ten records cannot
	// fit in fewer than three transactions.
	h.applier.rowsPer = statelog.ApplyTxRowBudget/3 + 1
	const records = 10
	for seq := uint64(1); seq <= records; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprintf("o%d", seq), fmt.Sprintf("op-%d", seq), 1))
	}
	if err := h.run(records); err != nil {
		t.Fatalf("run: %v", err)
	}

	seen := h.applier.seen()
	if len(seen) != records {
		t.Fatalf("the applier saw %d record(s), want %d", len(seen), records)
	}
	for i := range seen {
		if seen[i].Seq != uint64(i+1) {
			t.Fatalf("the applier saw %v — records apply in strictly increasing "+
				"position, contiguously, and a transaction boundary is not a "+
				"place the order may change", seen)
		}
	}
	// AND IT REALLY DID SPLIT. A case that fitted everything in one
	// transaction asserts nothing about a boundary.
	h.applier.mu.Lock()
	commits := h.applier.commits
	h.applier.mu.Unlock()
	if commits < 3 {
		t.Fatalf("%d transaction(s) for %d records at %d rows each against a "+
			"budget of %d — the split never happened, so this case proved "+
			"nothing about a boundary",
			commits, records, h.applier.rowsPer, statelog.ApplyTxRowBudget)
	}
}
