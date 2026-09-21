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
	"github.com/crewlet/crewlet/internal/statelog/metrics"
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

	// withhold is how many queued records the broker keeps back from
	// every fetch while still counting them as pending — which is what a
	// consumer at its in-flight ceiling looks like from the loop: Pending
	// says records remain and Fetch hands over none of them.
	withhold int

	// failures is how many fetches the broker answers with an error
	// before it answers normally again — a blip, as the loop sees one.
	failures int

	// after runs once, after the fetch with that NUMBER has handed its
	// records over. It is how a case stages a REDELIVERY, which no single
	// batch can: a batch is sorted and deduped on arrival, so a record
	// that comes back has to arrive in a LATER fetch, while the loop still
	// holds a higher one in the same run.
	after map[int]func()
}

// afterFetch schedules a hook to run once the nth fetch has answered.
func (f *probeFetch) afterFetch(n int, hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.after == nil {
		f.after = map[int]func(){}
	}
	f.after[n] = hook
}

func newProbeFetch() *probeFetch { return &probeFetch{acked: map[uint64]int{}} }

// offer queues one record at seq, encoded as the probe domain's envelope and
// SIGNED, because what a broker hands an applier is what a publisher wrote and
// the framework signs every one of those. A fixture that offered an unsigned
// record would be testing the refusal rather than the path.
func (f *probeFetch) offer(seq uint64, env statelog.Envelope) {
	body, err := json.Marshal(env)
	if err != nil {
		panic(err)
	}
	body = probeSeal(body)
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
		if f.failures > 0 {
			f.failures--
			f.mu.Unlock()
			return nil, errors.New("the probe broker did not answer")
		}
		if deliverable := len(f.queue) - f.withhold; deliverable > 0 {
			n := min(deliverable, maxMessages)
			out := f.queue[:n]
			f.queue = f.queue[n:]
			hook := f.after[f.fetches]
			f.mu.Unlock()
			if hook != nil {
				// OUTSIDE THE LOCK, because a hook stages more
				// records and offering one takes it.
				hook()
			}
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

// fetchCount is how many times the loop asked.
func (f *probeFetch) fetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches
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
	metrics *metrics.Recorder
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
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain:     domain,
		Verifier:   testVerifier(t, domain),
		Applier:    applier,
		Fetch:      fetch,
		DB:         db.Replicated(),
		Generation: 1,
		Metrics:    recorder,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return &applyHarness{t: t, db: db, runner: runner, applier: applier,
		fetch: fetch, metrics: recorder}
}

// upgrade rebuilds the runner over the SAME database under another domain —
// the shape of a node restarting on a newer build. The applier, the fetch and
// the recorder are fresh, because a new process has new ones; the rows, the
// checkpoint and the retained records are what survive.
func (h *applyHarness) upgrade(domain statelog.Domain) {
	h.t.Helper()
	h.rebuild(domain, time.Time{})
}

// rebuild is [applyHarness.upgrade] under a stream identity: the instant the
// broker reports for the stream, which the loop compares against the instant
// its checkpoint was committed under.
func (h *applyHarness) rebuild(domain statelog.Domain, created time.Time) {
	h.t.Helper()
	h.applier = newProbeApplier()
	h.fetch = newProbeFetch()
	recorder, err := metrics.New()
	if err != nil {
		h.t.Fatalf("recorder: %v", err)
	}
	h.metrics = recorder
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain:          domain,
		Verifier:        testVerifier(h.t, domain),
		Applier:         h.applier,
		Fetch:           h.fetch,
		DB:              h.db.Replicated(),
		Generation:      1,
		StreamCreatedAt: created,
		Metrics:         recorder,
	})
	if err != nil {
		h.t.Fatalf("NewRunner: %v", err)
	}
	h.runner = runner
}

// retainedCount is how many records this node still holds that it could not
// decode.
func (h *applyHarness) retainedCount() int64 {
	h.t.Helper()
	var count int64
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT COUNT(*) FROM probe_log_deferred`).Scan(&count)
	}); err != nil {
		h.t.Fatalf("count the retained records: %v", err)
	}
	return count
}

// boot runs the loop with nothing queued until the retained table holds want
// records, or fails. It is how a reprocess is observed: the checkpoint does
// not move, so [applyHarness.run] has nothing to wait on.
func (h *applyHarness) boot(want int64) error {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), 20*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.retainedCount() == want {
			// Settle: the release and the applier's Committed hook
			// run in that order, and the count moves first.
			time.Sleep(20 * time.Millisecond)
			cancel()
			<-errs
			return nil
		}
		select {
		case err := <-errs:
			return err
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	<-errs
	return fmt.Errorf("%d record(s) are still retained, want %d", h.retainedCount(), want)
}

// counter is one instrument's total across every attribute set.
func (h *applyHarness) counter(name string) uint64 {
	h.t.Helper()
	var total uint64
	for _, snapshot := range h.metrics.Read() {
		if snapshot.Name == name {
			total += snapshot.Total
		}
	}
	return total
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

// AN APPLIER'S FAILURE ON ONE RECORD DOES NOT MOVE THE CHECKPOINT, AND DOES
// NOT END THE LOOP.
//
// The transaction rolls back, so the rows, the anchor, the operation id and
// the checkpoint all go back together — which is the whole point of committing
// them together, and the property a node "can only be behind, never
// inconsistent" rests on. And the loop retries the same run in place: a
// failure that is not a stop is a disk that refused, a broker that did not
// answer, an applier that errored — none of which says this node cannot run
// the company's records, and a loop that returned on one of them left the
// domain dead for the life of the process. Once the failure clears, both
// records apply.
func TestAFailedApplyLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.applier.failAt = 2
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()

	// The failure is retried, quietly: inside the budget the fault is not
	// reported, past it the same fault is.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, faulted := h.runner.Fault(time.Now().Add(statelog.ApplyRetryBudget)); faulted {
			break
		}
		select {
		case err := <-errs:
			t.Fatalf("Run returned %v on a failure that is not a stop — the domain "+
				"is dead for the life of the process", err)
		case <-time.After(2 * time.Millisecond):
		}
	}
	msg, faulted := h.runner.Fault(time.Now().Add(statelog.ApplyRetryBudget))
	if !faulted || !strings.Contains(msg, "refuses sequence 2") {
		t.Fatalf("Fault past the budget = (%q, %v), want the applier's own refusal", msg, faulted)
	}
	if _, faulted := h.runner.Fault(time.Now()); faulted {
		t.Fatal("the fault is reported inside the retry budget, so a single " +
			"refused transaction would move a company's seats")
	}
	if err := h.runner.Stopped(); err != nil {
		t.Fatalf("a retried failure reads as a stop: %v", err)
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

	// THE FAILURE CLEARS, and the same run — never re-fetched, never left
	// to a redelivery — applies whole.
	h.applier.mu.Lock()
	h.applier.failAt = 0
	h.applier.mu.Unlock()
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && h.runner.Committed().Seq < 2 {
		time.Sleep(2 * time.Millisecond)
	}
	got := h.runner.Committed().Seq
	cancel()
	<-errs
	if got != 2 {
		t.Fatalf("the checkpoint is at %d after the failure cleared, want 2", got)
	}
	if _, faulted := h.runner.Fault(time.Now().Add(statelog.ApplyRetryBudget)); faulted {
		t.Fatal("the fault is still reported after a retry succeeded")
	}
	if h.fetch.ackCount(1) != 1 || h.fetch.ackCount(2) != 1 {
		t.Fatalf("the records were acknowledged %d and %d time(s), want once each",
			h.fetch.ackCount(1), h.fetch.ackCount(2))
	}
	if got := h.counter(metrics.StatelogApplyRetries); got == 0 {
		t.Fatal("the retries were not counted, so an operator watching the " +
			"instrument would see a healthy loop")
	}
}

// A BROKER THAT DOES NOT ANSWER IS RETRIED, NOT RETURNED FROM.
//
// A fetch error was the loop's exit: one timeout at the wrong moment and the
// domain's applier was gone until the process restarted, with the first
// symptom a node that had lost its seats. A blip is waited out on a widening
// pause and the loop carries on from where it was.
func TestABrokerBlipDoesNotEndTheApplier(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.mu.Lock()
	h.fetch.failures = 3
	h.fetch.mu.Unlock()
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := h.runner.Stopped(); err != nil {
		t.Fatalf("three failed fetches stopped the applier: %v", err)
	}
	if got := h.fetch.fetchCount(); got < 4 {
		t.Fatalf("the loop asked %d time(s), so the failures were not what it "+
			"waited out", got)
	}
	if _, faulted := h.runner.Fault(time.Now().Add(statelog.ApplyRetryBudget)); faulted {
		t.Fatal("a blip the loop recovered from is still reported as a fault")
	}
}

// THE PAUSE DOUBLES TO THE CEILING and never past it, and the ceiling leaves a
// fault several attempts before it is reported — so one failed call never
// sheds a seat.
func TestTheRetryPauseIsBoundedAndTheBudgetOutlastsSeveralOfIt(t *testing.T) {
	t.Parallel()
	if statelog.ApplyRetryBeat != statelog.ApplyLinger {
		t.Errorf("the retry beat is %v against a linger of %v — a retry inside the "+
			"linger is indistinguishable from an ordinary partial batch",
			statelog.ApplyRetryBeat, statelog.ApplyLinger)
	}
	if statelog.ApplyRetryCeiling*6 > statelog.ApplyRetryBudget {
		t.Errorf("the ceiling %v leaves fewer than six attempts inside the %v budget",
			statelog.ApplyRetryCeiling, statelog.ApplyRetryBudget)
	}
	if statelog.ApplyRetryBudget >= statelog.StallGrace {
		t.Errorf("the retry budget %v is not inside the stall grace %v, so a node "+
			"could report itself healthy for longer than it made no progress",
			statelog.ApplyRetryBudget, statelog.StallGrace)
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

// THE OPERATION LEDGER IS SWEPT, and nothing swept it.
//
// Every `<domain>_ops` migration says the table is swept and ships
// `<domain>_ops_swept_idx` for the range delete, and no code anywhere deleted
// a row: one per applied record, kept for ever, on every node. What makes it
// invisible rather than loud is that the table's only reader asks "did my
// operation land", which nobody asks about a month-old op id — so the answers
// stay correct while the file grows.
//
// The horizon is the CLIENT'S rather than the machine's: a seat carries an op
// id forward and re-asks on its next wake, hours or a weekend later, and an op
// id that outlives its row resolves `unknown` rather than `applied` — which
// sends a turn to re-decide work it already did.
func TestTheOperationLedgerIsSwept(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	for seq := uint64(1); seq <= 6; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprintf("o%d", seq),
			fmt.Sprintf("op-%d", seq), 1))
	}
	if err := h.run(6); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, held, err := h.runner.Op(t.Context(), "op-3"); err != nil || !held {
		t.Fatalf("op-3 is not in the ledger after applying it (held=%v, %v)",
			held, err)
	}

	// A CUTOFF IN THE FUTURE sweeps everything, which is the arithmetic
	// rather than the horizon: what is under test is that the delete
	// happens and reports what it did.
	swept, err := h.runner.PurgeOps(t.Context(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("PurgeOps: %v", err)
	}
	if swept != 6 {
		t.Errorf("the sweep deleted %d of 6 operation rows", swept)
	}
	if _, held, err := h.runner.Op(t.Context(), "op-3"); err != nil || held {
		t.Errorf("op-3 survived a sweep past its own instant (held=%v, %v)",
			held, err)
	}

	// AND THE CONTROL, because a sweep that deletes everything is not a
	// sweep — it is a truncate with a cutoff argument. A row inside the
	// horizon stays.
	h.fetch.offer(7, env(7, "edit", "o7", "op-7", 1))
	if err := h.run(7); err != nil {
		t.Fatalf("run: %v", err)
	}
	swept, err = h.runner.PurgeOps(t.Context(), time.Now().Add(-statelog.OpsRetention))
	if err != nil {
		t.Fatalf("PurgeOps: %v", err)
	}
	if swept != 0 {
		t.Errorf("a sweep at the real horizon deleted %d row(s) written "+
			"moments ago", swept)
	}
	if _, held, err := h.runner.Op(t.Context(), "op-7"); err != nil || !held {
		t.Errorf("op-7 was swept inside its own retention (held=%v, %v)",
			held, err)
	}
}

// A BACKLOG WIDER THAN ONE BATCH STILL DRAINS.
//
// The sweep is batched because the applier's connection is pinned and its
// commits are the same file's: one statement over a month of rows holds the
// writer for as long as it takes, and the backlog case — a node returning from
// a long absence — is exactly the one that matters. A loop that stopped after
// its first batch would look identical on a small table and leave the month
// behind on the one node that had it.
func TestTheOperationSweepDrainsABacklogWiderThanOneBatch(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	// Written directly: applying enough records to cross the batch is a
	// minute of broker round trips to test one loop.
	rows := statelog.OpsPurgeBatch + 7
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range rows {
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO probe_ops (op_id, subject, position, applied_at)
				VALUES (?, 'probe.o1', 1, 0)`, fmt.Sprintf("op-%d", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the ledger: %v", err)
	}

	swept, err := h.runner.PurgeOps(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("PurgeOps: %v", err)
	}
	if swept != int64(rows) {
		t.Errorf("the sweep deleted %d of %d rows — a loop that stops at its "+
			"first batch leaves the backlog on the one node that had one",
			swept, rows)
	}
	var left int
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM probe_ops`).Scan(&left)
	}); err != nil {
		t.Fatalf("count what is left: %v", err)
	}
	if left != 0 {
		t.Errorf("%d operation rows survive a sweep past every one of them", left)
	}
}

// A CLEAN APPLY COUNTS NO TRANSACTION ABORTS.
//
// `apply.tx.aborts` is the number that says whether an apply's body is ever
// run twice on the operator's own hardware, and it reads zero by
// construction: the store's write transactions hold the file's lock from
// their BEGIN, so nothing committing elsewhere in the file can abort one. A
// non-zero count means the retry budget is being spent rather than held in
// reserve.
//
// An instrument that recorded on every run would be useless in the direction
// that matters — the reserve would read as spent on a healthy fleet — and it
// is one off-by-one away, because what the applier counts is its own re-runs
// and a first attempt is not a retry.
func TestApplyTxAbortsAreCounted(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	for seq := uint64(1); seq <= 5; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprintf("o%d", seq),
			fmt.Sprintf("op-%d", seq), 1))
	}
	if err := h.run(5); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.counter(metrics.StatelogApplyTxAborts); got != 0 {
		t.Errorf("a clean apply counted %d transaction abort(s) — the retry "+
			"budget reads as spent on a fleet that has not spent any of it",
			got)
	}
	// THE CONTROL, on the same recorder and the same instrument: the
	// assertion above passes identically when nothing is wired at all,
	// and this is what separates the two.
	if got := h.counter(metrics.StatelogApplyRecords); got == 0 {
		t.Error("the applier recorded no records at all, so this recorder is " +
			"not connected and the assertion above proves nothing")
	}
}

// TestTheApplierMeasuresItsOwnDrain is the input three answers divide a record
// backlog by to state a TIME: a bounded stale read's "am I inside the caller's
// staleness", a refusal's `retry_after_seconds`, and the apply-lag alarm.
//
// Unmeasured, all three fall back to one record per second — so a node two
// thousand records behind, which is about a second of real work, reports
// itself half an hour behind, refuses reads it should have served and fires an
// alarm nobody can act on.
func TestTheApplierMeasuresItsOwnDrain(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	if got := h.runner.Drain(); got != 0 {
		t.Fatalf("a loop that has applied nothing reports a drain of %v — zero "+
			"means unmeasured, and every caller falls back to a floor rather "+
			"than dividing by a guess", got)
	}
	for seq := uint64(1); seq <= 20; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprintf("o%d", seq),
			fmt.Sprintf("op-%d", seq), 1))
	}
	if err := h.run(20); err != nil {
		t.Fatalf("run: %v", err)
	}
	drain := h.runner.Drain()
	if drain <= 0 {
		t.Fatalf("after applying twenty records the drain is %v — an unmeasured "+
			"rate makes every lag figure in the engine a count in disguise",
			drain)
	}
	// A SANITY BOUND rather than a threshold: what is being asserted is
	// that the figure is a RATE — records divided by the time they took —
	// and not a count, a constant, or the one-per-second fallback.
	if drain < 1 {
		t.Fatalf("the drain is %v records/second over twenty records applied in "+
			"a test — that is the unmeasured fallback rather than a "+
			"measurement", drain)
	}

	// AND THE COMMIT RATE BESIDE IT, which is a different resource: rows
	// per second is progress, commits per second is the FSYNC rate a
	// device's write budget is spent by. They move independently by
	// design — a run is filled toward the transaction budget precisely so
	// a barrier-heavy stream commits once per two dozen records — so a
	// node whose rows/s is healthy and whose commits/s has doubled is
	// doing twice the disk work for the same progress, and neither figure
	// alone can say it.
	commits := h.runner.Commits()
	if commits <= 0 {
		t.Fatalf("after applying twenty records the commit rate is %v, so the "+
			"gauge an operator compares against their device's committed "+
			"write rate never appears", commits)
	}
	if commits > drain {
		t.Errorf("the commit rate (%v/s) is above the record rate (%v/s), "+
			"which cannot happen: a run is one transaction over at least one "+
			"record", commits, drain)
	}
}

// A PARTIAL RUN COMMITS WHEN THE BROKER HANDS OVER NOTHING.
//
// The loop fills a run toward its budget while records are pending, and the
// broker reports records pending for as long as it has not DELIVERED them —
// which includes records it is deliberately withholding because the consumer
// is at its in-flight ceiling. That ceiling is reached by exactly the records
// the run holds, and it clears only when they are acknowledged, which happens
// only after the run commits. A loop that kept pulling while anything was
// pending was therefore waiting on its own commit, for ever: measured on the
// embedded broker, a 257-record backlog stopped a node applying anything at
// all. The linger is the bound: a pull that returns nothing inside it closes
// the run.
func TestAPartialRunCommitsWhenTheBrokerHandsOverNothing(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	for seq := uint64(1); seq <= 3; seq++ {
		h.fetch.offer(seq, env(seq, "edit", string(rune('a'+seq-1)), fmt.Sprintf("op-%d", seq), 1))
	}
	// The broker withholds the third: it stays pending and is never
	// delivered until the first two are acknowledged.
	h.fetch.mu.Lock()
	h.fetch.withhold = 1
	h.fetch.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()

	// Two records, one linger: the run commits inside a couple of lingers
	// rather than never.
	deadline := time.Now().Add(4 * statelog.ApplyLinger)
	for time.Now().Before(deadline) && h.runner.Committed().Seq < 2 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := h.runner.Committed().Seq; got != 2 {
		cancel()
		<-errs
		t.Fatalf("the checkpoint is at %d after %s with two records in hand and "+
			"one withheld, want 2 — a run that waits for a pending record the "+
			"broker will not deliver until the run commits waits for ever",
			got, 4*statelog.ApplyLinger)
	}
	if h.fetch.ackCount(1) != 1 || h.fetch.ackCount(2) != 1 {
		cancel()
		<-errs
		t.Fatalf("the committed records were acknowledged %d and %d time(s), "+
			"want once each", h.fetch.ackCount(1), h.fetch.ackCount(2))
	}

	// Acknowledged, the broker releases the third and the loop takes it.
	h.fetch.mu.Lock()
	h.fetch.withhold = 0
	h.fetch.mu.Unlock()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.runner.Committed().Seq < 3 {
		time.Sleep(2 * time.Millisecond)
	}
	got := h.runner.Committed().Seq
	cancel()
	<-errs
	if got != 3 {
		t.Fatalf("the checkpoint is at %d after the broker released the third "+
			"record, want 3", got)
	}
}

// upgradedDomain is the probe domain as a newer build reads it.
type upgradedDomain struct {
	probeDomain
	reads int
}

func (d upgradedDomain) RecordVersion() int { return d.reads }

// A RETAINED RECORD IS APPLIED BY THE BUILD THAT CAN READ IT, at its next
// boot, in log order, and released in the transaction that applied it.
//
// This is the second half of the retain rule and the half that had no code:
// a record this build cannot decode is kept byte for byte so that a build
// which can decode it applies it later. Without the later, a node that sat
// through a rolling upgrade kept its deferrals after it was upgraded — refusing
// every read and write about the objects they covered, for ever, and naming a
// record version it was already running.
//
// The record retained BECAUSE its scope met the undecodable one is applied
// too, and after it: it was decodable all along, and what kept it back was
// order.
func TestARetainedRecordIsAppliedByTheBuildThatCanReadIt(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	// THE ARBITRATED KIND, so the records carry an anchor to check.
	h.fetch.offer(1, env(1, "object", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "object", "b", "op-2", 9)) // above this build
	h.fetch.offer(3, env(3, "object", "b", "op-3", 1)) // on the rows 2 left stale
	h.fetch.offer(4, env(4, "object", "c", "op-4", 1))
	if err := h.run(4); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.retainedCount(); got != 2 {
		t.Fatalf("the old build retained %d record(s), want 2", got)
	}

	// THE UPGRADE. Same database, a build that reads version 9.
	h.upgrade(upgradedDomain{reads: 9})
	if err := h.boot(0); err != nil {
		t.Fatalf("the upgraded build's boot: %v", err)
	}

	seen := h.applier.seen()
	if len(seen) != 2 || seen[0].Seq != 2 || seen[1].Seq != 3 {
		t.Fatalf("the upgraded build applied %v, want the retained records at 2 "+
			"then 3 — in log order, and the one held back by scope after the one "+
			"that held it", seen)
	}
	if _, held := h.runner.Deferred(); held {
		t.Fatal("the runner still reports a deferred record after applying them all")
	}
	if got := h.runner.Committed().Seq; got != 4 {
		t.Fatalf("the checkpoint moved to %d — a reprocess applies at the "+
			"original positions and the log consumed them long ago", got)
	}
	var ops, orphans int64
	var anchor int64
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM probe_ops WHERE op_id IN ('op-2', 'op-3')`).
			Scan(&ops); err != nil {
			return err
		}
		if err := tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM probe_deferred_scope`).Scan(&orphans); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(),
			`SELECT anchor FROM statelog_anchor WHERE subject = ?`,
			probePrefix+".object.b").Scan(&anchor)
	}); err != nil {
		t.Fatalf("read the tables: %v", err)
	}
	if ops != 2 {
		t.Fatalf("%d operation row(s) for the reprocessed records, want 2 — a "+
			"caller resolving op-2 would read its absence as somebody else winning", ops)
	}
	if orphans != 0 {
		t.Fatalf("%d scope row(s) outlived their records", orphans)
	}
	if want := (statelog.Position{Stream: probeStream, Generation: 1, Seq: 3}).Packed(); anchor != want {
		t.Fatalf("the anchor on object.b is %d, want %d — a replay at the original "+
			"position must not move it backwards", anchor, want)
	}
	if got := h.counter(metrics.StatelogApplyRecords); got != 2 {
		t.Fatalf("the apply counter recorded %d, want the 2 reprocessed", got)
	}
}

// A REPROCESS STOPS AT WHAT IT STILL CANNOT READ, and keeps everything that
// record covers — however many builds it waits through.
func TestAReprocessKeepsWhatAnUnreadableRecordStillCovers(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 9))  // readable at 9
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 12)) // not yet
	h.fetch.offer(3, env(3, "edit", "b", "op-3", 1))  // covered by 2
	h.fetch.offer(4, env(4, "edit", "a", "op-4", 1))  // covered by 1
	if err := h.run(4); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.retainedCount(); got != 4 {
		t.Fatalf("the old build retained %d record(s), want 4", got)
	}

	h.upgrade(upgradedDomain{reads: 9})
	if err := h.boot(2); err != nil {
		t.Fatalf("the build reading 9: %v", err)
	}
	seen := h.applier.seen()
	if len(seen) != 2 || seen[0].Seq != 1 || seen[1].Seq != 4 {
		t.Fatalf("the build reading 9 applied %v, want 1 then 4: 2 is still above "+
			"it and 3 is covered by 2", seen)
	}
	d, held := h.runner.Deferred()
	if !held || d.Position.Seq != 2 || d.Version != 12 {
		t.Fatalf("the runner reports %+v held=%v, want the record at 2 needing "+
			"version 12", d, held)
	}

	// AND THE NEXT UPGRADE FINISHES IT.
	h.upgrade(upgradedDomain{reads: 12})
	if err := h.boot(0); err != nil {
		t.Fatalf("the build reading 12: %v", err)
	}
	seen = h.applier.seen()
	if len(seen) != 2 || seen[0].Seq != 2 || seen[1].Seq != 3 {
		t.Fatalf("the build reading 12 applied %v, want 2 then 3", seen)
	}
}

// A RECREATED STREAM STOPS THE APPLIER, and it is the broker's own creation
// instant that says so.
//
// A stream deleted and remade restarts its sequences at one. Every position
// this node holds then names a number space that no longer exists, and a
// consumer resumed from the checkpoint waits for a sequence the new stream
// reaches only by coincidence — reporting nothing pending, looking perfectly
// caught up, applying none of the new stream's records. The instant is the
// only thing that can notice: the sequences are plausible and an empty stream
// and an emptied one have the same count.
//
// Until the engine passed the LIVE instant, the loop compared the checkpoint
// row's own recorded value against itself and this could never fire.
func TestARecreatedStreamStopsTheApplier(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	born := time.Date(2026, 9, 10, 12, 0, 0, 123_456_789, time.UTC)
	h.rebuild(probeDomain{}, born)
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}

	// THE SAME STREAM, reported at nanosecond precision against a row that
	// keeps microseconds: the same stream, and it must read as such.
	h.rebuild(probeDomain{}, born.Add(100*time.Nanosecond))
	if err := h.boot(0); err != nil {
		t.Fatalf("the same stream, reported at a finer resolution, was refused: %v", err)
	}
	if err := h.runner.Stopped(); err != nil {
		t.Fatalf("the same stream stopped the applier: %v", err)
	}

	// A DIFFERENT STREAM WEARING THE SAME NAME.
	h.rebuild(probeDomain{}, born.Add(time.Hour))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := h.runner.Run(ctx)
	if !errors.Is(err, statelog.ErrStopped) || !errors.Is(err, statelog.ErrStreamRecreated) {
		t.Fatalf("Run on a recreated stream returned %v, want a stop naming the "+
			"recreation", err)
	}
	if stopped := h.runner.Stopped(); stopped == nil {
		t.Fatal("the applier does not report itself stopped, so its health would " +
			"not refuse and its seats would not move")
	}
	if !strings.Contains(err.Error(), "reanchor") {
		t.Fatalf("the stop does not name the verb that repairs it: %v", err)
	}
	if got := h.runner.Committed().Seq; got != 2 {
		t.Fatalf("the checkpoint moved to %d on a stopped applier", got)
	}
}

// A RUNNER RUNS AGAIN FROM THE CHECKPOINT IT NOW HOLDS.
//
// An adoption on a running node ends every apply loop, replaces the file, and
// starts the loops again — the same runners, because every subsystem holds
// them. So Run has to be re-enterable: it resumes from whatever checkpoint the
// file keeps, and a stop from the previous run is a verdict about rows this
// node no longer has rather than something to remember.
func TestARunnerRunsAgainFromTheCheckpointItNowHolds(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	if err := h.run(2); err != nil {
		t.Fatalf("the first run: %v", err)
	}

	// A caller left waiting across the gap is told, not released.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	waited := make(chan error, 1)
	go func() {
		waited <- h.runner.WaitCommitted(ctx, statelog.Position{Stream: probeStream, Generation: 1, Seq: 3})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && h.runner.Waiting() == 0 {
		time.Sleep(time.Millisecond)
	}
	runCtx, stopRun := context.WithCancel(t.Context())
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(runCtx) }()
	time.Sleep(50 * time.Millisecond)
	stopRun()
	<-errs
	select {
	case err := <-waited:
		if !errors.Is(err, statelog.ErrWaitAbandoned) {
			t.Fatalf("the waiter across the gap got %v, want %v", err, statelog.ErrWaitAbandoned)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter was not told the loop ended")
	}

	// THE SECOND RUN continues from 2.
	h.fetch.offer(3, env(3, "edit", "c", "op-3", 1))
	if err := h.run(3); err != nil {
		t.Fatalf("the second run: %v", err)
	}
	seen := h.applier.seen()
	if len(seen) != 3 || seen[2].Seq != 3 {
		t.Fatalf("the applier saw %v across two runs, want 1, 2, 3 once each", seen)
	}
}

// A LATE REDELIVERY MUST NOT MOVE THE CHECKPOINT BACKWARDS.
//
// [reorderBuffer.admit] deliberately hands a record already below the run's
// high-water mark straight to the caller, because a redelivery nothing
// acknowledges is redelivered for ever. A run therefore closes legitimately as
// [1, 2, 1] — and the checkpoint is the HIGHEST position it applied, never the
// tail.
//
// Checkpointing the tail wrote 1 in the same transaction that committed 2's
// rows: a cursor understating its own database, which is the one thing the
// checkpoint exists to rule out. It is not a self-correcting slip either. The
// waiters release through the same value, so a linearizable read waiting for 2
// is refused `behind` over rows this node already holds; and the next boot
// resumes at 2 and re-applies a record whose anchor it already advanced past.
func TestALateRedeliveryDoesNotRegressTheCheckpoint(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "object", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "object", "b", "op-2", 1))
	// THE ACKNOWLEDGEMENT OF 1 WAS LOST, so the broker hands it back
	// while the loop still holds 2 — in a later fetch, because a single
	// batch is deduped on arrival and could never produce this run.
	h.fetch.afterFetch(1, func() {
		h.fetch.offer(1, env(1, "object", "a", "op-1", 1))
	})

	ctx, cancel := context.WithTimeout(h.t.Context(), 20*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()

	// THE ACK IS THE SIGNAL THE RUN COMMITTED, and it happens whatever
	// the checkpoint ended up saying — so a broken checkpoint fails these
	// assertions promptly rather than waiting out a timeout.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && h.fetch.ackCount(2) == 0 {
		select {
		case err := <-errs:
			t.Fatalf("the applier stopped before the run committed: %v", err)
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	<-errs

	if got := h.runner.Committed().Seq; got != 2 {
		t.Errorf("the loop reports itself committed through %d, want 2 — the "+
			"run applied 2 and closed with a redelivery of 1", got)
	}
	at, _, found, err := statelog.CursorFor(h.t.Context(), h.db.Replicated(),
		probeDomain{}.Stream().Name)
	if err != nil {
		t.Fatalf("read the checkpoint: %v", err)
	}
	if !found || at.Seq != 2 {
		t.Errorf("the checkpoint row is at %s (found=%v), want sequence 2 — a "+
			"cursor below rows its own transaction committed is the one state "+
			"the checkpoint exists to rule out", at, found)
	}
	// AND THE REDELIVERY IS STILL ACKNOWLEDGED, which is why admit passes
	// it through at all: dropping it to protect the checkpoint would
	// leave the broker redelivering it for ever.
	if got := h.fetch.ackCount(1); got != 2 {
		t.Errorf("sequence 1 was acknowledged %d time(s), want 2 — the "+
			"original delivery and the redelivery", got)
	}
}

// flakyEstate is a replicated estate whose connection cannot be pinned for the
// first `refusals` attempts — a store that is momentarily unavailable, which is
// what an adoption's close-and-reopen bracket and a refused transaction both
// look like from the applier.
type flakyEstate struct {
	inner interface {
		Read(context.Context, func(*sql.Tx) error) error
		Tx(context.Context, func(*sql.Tx) error) error
		Writer(context.Context) (*store.Writer, error)
		Caps() store.Capabilities
	}
	mu       sync.Mutex
	refusals int
	attempts int
}

func (f *flakyEstate) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	return f.inner.Read(ctx, fn)
}

func (f *flakyEstate) Caps() store.Capabilities { return f.inner.Caps() }

func (f *flakyEstate) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	return f.inner.Tx(ctx, fn)
}

func (f *flakyEstate) Writer(ctx context.Context) (*store.Writer, error) {
	f.mu.Lock()
	f.attempts++
	refuse := f.refusals > 0
	if refuse {
		f.refusals--
	}
	f.mu.Unlock()
	if refuse {
		return nil, errors.New("the probe store is not available")
	}
	return f.inner.Writer(ctx)
}

func (f *flakyEstate) pinAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// A STORE THAT IS MOMENTARILY UNAVAILABLE AT STARTUP IS RETRIED, NOT FATAL.
//
// Pinning the connection, reading the checkpoint and reprocessing what an
// earlier build retained are all database work, and all three used to sit
// ABOVE the retry loop: a store that refused for a moment — the adoption
// bracket between a close and a reopen, a refused transaction, a slow disk —
// returned straight out of Run and left the domain with no applier for the
// life of the process.
//
// Nothing restarted it and nothing reported it either. Stopped stayed nil and
// no fault was recorded, so this node went on publishing a caught-up position
// for a domain that would never apply another record — which is precisely the
// failure the loop's retry design exists to rule out, reached through the door
// above it.
func TestAStoreThatRefusesAtStartupIsRetried(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	flaky := &flakyEstate{inner: h.db.Replicated(), refusals: 3}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain:     probeDomain{},
		Verifier:   testVerifier(t, probeDomain{}),
		Applier:    h.applier,
		Fetch:      h.fetch,
		DB:         flaky,
		Generation: 1,
		Metrics:    h.metrics,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	h.fetch.offer(1, env(1, "object", "a", "op-1", 1))

	ctx, cancel := context.WithTimeout(h.t.Context(), 30*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- runner.Run(ctx) }()

	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) && runner.Committed().Seq < 1 {
		select {
		case err := <-errs:
			t.Fatalf("Run returned %v — a store that refused three times took "+
				"the domain's applier down for the life of the process, with "+
				"Stopped unset and no fault recorded", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-errs

	if got := runner.Committed().Seq; got != 1 {
		t.Fatalf("the applier reached %d after the store came back, want 1", got)
	}
	if got := flaky.pinAttempts(); got < 4 {
		t.Errorf("the connection was pinned %d time(s), want at least 4 — three "+
			"refusals and the attempt that succeeded", got)
	}
	if err := runner.Stopped(); err != nil {
		t.Errorf("the applier reports itself stopped after recovering: %v", err)
	}
}

// probeSeal signs a fixture's record under the same keyring the harness
// verifies with. It panics rather than returning an error: a fixture that
// cannot sign is a broken test file, not a case.
func probeSeal(body []byte) []byte {
	signer, err := statelog.NewSigner(probeDomain{}.Name(), testRing())
	if err != nil {
		panic(err)
	}
	return signer.Seal(body)
}
