package statelog_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// ---- the applier's fakes -------------------------------------------- //
//
// The loop under test is decided by what these answer, so a fake driven into
// each branch tests more than a real domain that reaches only the easy ones.

// probeApplier writes one row per record into a table the test can count.
type probeApplier struct {
	mu      sync.Mutex
	applied []statelog.Position
	epochs  []map[string]any
	commits int
	gate    statelog.Reason
	gated   map[uint64]bool
	asked   map[uint64]int
	failAt  uint64
	rowsPer int

	// lockedAt is a sequence whose FIRST apply fails as a write that lost
	// the file's lock does, so the store re-runs the transaction's body: a
	// re-run no healthy store produces. Zero fails nothing.
	lockedAt uint64

	// hooks is every post-commit call, in order: "retained N" per
	// [statelog.RetentionHook] call, "committed" per
	// [statelog.Applier.Committed].
	hooks []string

	// instants is the broker instant each applied record was handed, by
	// sequence, on the record and in the options.
	instants map[uint64]probeInstant
}

// probeInstant is the broker instant one apply was handed, twice over: on the
// record ([statelog.Record.StoredAt]) and in the options
// ([statelog.ApplyOptions.StoredAt]).
type probeInstant struct {
	record, options time.Time
}

// The framework finds the optional hook by a type assertion, so an applier
// that takes it asserts so.
var _ statelog.RetentionHook = (*probeApplier)(nil)

func newProbeApplier() *probeApplier {
	return &probeApplier{gated: map[uint64]bool{}, asked: map[uint64]int{}, rowsPer: 1,
		instants: map[uint64]probeInstant{}}
}

func (a *probeApplier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record, opts statelog.ApplyOptions) (int, error) {
	a.mu.Lock()
	fail, per := a.failAt, a.rowsPer
	locked := a.lockedAt != 0 && rec.Position.Seq == a.lockedAt
	if locked {
		a.lockedAt = 0
	}
	a.mu.Unlock()
	if fail != 0 && rec.Position.Seq == fail {
		return 0, fmt.Errorf("the probe applier refuses sequence %d", fail)
	}
	if locked {
		return 0, fmt.Errorf("the probe applier at sequence %d: database is locked",
			rec.Position.Seq)
	}
	if rec.Kind == statelog.BarrierKind {
		// A BARRIER WRITES NO ROW (statelogtest.ABarrierWritesNoRow): the
		// loop serves the reads waiting on it before its transaction commits.
		a.mu.Lock()
		a.applied = append(a.applied, rec.Position)
		a.epochs = append(a.epochs, opts.Epoch)
		a.mu.Unlock()
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO probe_rows (position, kind, stored_at) VALUES (?, ?, ?)
		 ON CONFLICT (position) DO NOTHING`,
		rec.Position.Packed(), rec.Kind, store.EncodeTime(opts.StoredAt)); err != nil {
		return 0, err
	}
	a.mu.Lock()
	a.applied = append(a.applied, rec.Position)
	a.epochs = append(a.epochs, opts.Epoch)
	a.instants[rec.Position.Seq] = probeInstant{record: rec.StoredAt, options: opts.StoredAt}
	a.mu.Unlock()
	return per, nil
}

func (a *probeApplier) Gated(_ context.Context, _ *sql.Tx, rec statelog.Record) (statelog.Reason, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked[rec.Position.Seq]++
	return a.gate, a.gated[rec.Position.Seq], nil
}

func (a *probeApplier) Committed(context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commits++
	a.hooks = append(a.hooks, "committed")
}

func (a *probeApplier) Retained(_ context.Context, records int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hooks = append(a.hooks, fmt.Sprintf("retained %d", records))
}

// hooksSeen is every post-commit call so far, in order.
func (a *probeApplier) hooksSeen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.hooks...)
}

func (a *probeApplier) seen() []statelog.Position {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]statelog.Position(nil), a.applied...)
}

// instantAt is the broker instant the apply at seq was handed, and false for a
// sequence this applier never applied.
func (a *probeApplier) instantAt(seq uint64) (probeInstant, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	at, applied := a.instants[seq]
	return at, applied
}

// epochsSeen is the epoch each applied record was handed, in apply order.
func (a *probeApplier) epochsSeen() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string]any(nil), a.epochs...)
}

// probeFetch hands the loop whatever the test queued and records what was
// acknowledged. As the log read by position ([statelog.CheckpointLog]) it keeps
// every record ever offered readable at its sequence, as a stream does.
type probeFetch struct {
	mu    sync.Mutex
	queue []statelog.Message
	acked map[uint64]int
	// fetches counts the loop's calls of Fetch, and answered those that
	// returned: one per CALL however long it waits, so "has the loop
	// fetched again" is not answered by the fetch it is already in.
	fetches, answered int

	// stopped is the runner's [statelog.Runner.Stopped], where a harness
	// built one: a halted runner fetches no more, so
	// [probeFetch.awaitAnswered] fails at once rather than at its bound.
	stopped func() error

	// released counts the deliveries the loop handed back unconsumed, by
	// sequence ([statelog.Message.Release]); redeliver is what the next
	// fetch owes for them ahead of the queue, as a broker answers a negative
	// acknowledgement: the record the log holds at each sequence NOW, which
	// a restore may have replaced.
	released  map[uint64]int
	redeliver []statelog.Message

	// log is every record offered, by sequence; a later offer REPLACES the
	// earlier, as a broker restored from an older copy and written past a
	// node's rows looks to that node. end, when set, is the last sequence
	// Bounds reports in place of the highest offered.
	log map[uint64]statelog.Message
	end *uint64

	// withhold is how many queued records every fetch keeps back while
	// Pending still counts them: a consumer at its in-flight ceiling.
	withhold int

	// failures is how many fetches fail before the broker answers again.
	failures int

	// after runs once, after the fetch with that NUMBER hands over its
	// records: how a case stages a REDELIVERY, which must arrive in a LATER
	// fetch because a batch is sorted and deduped on arrival.
	after map[int]func()

	// atHook runs once, after the next [probeFetch.At] has read its record:
	// how a case lands something between a verification's read at the
	// checkpoint and what the runner does with it.
	atHook func()
}

// afterAt schedules hook to run once, after the next read of the log by
// position.
func (f *probeFetch) afterAt(hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.atHook = hook
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

func newProbeFetch() *probeFetch {
	return &probeFetch{acked: map[uint64]int{}, released: map[uint64]int{},
		log: map[uint64]statelog.Message{}}
}

// probeStoredAt is the broker instant the probe log stamps the record at seq
// with.
func probeStoredAt(seq uint64) time.Time {
	return time.Unix(1_700_000_000, 0).UTC().Add(time.Duration(seq) * time.Second)
}

// offer queues one record at seq as the probe domain's envelope, SIGNED as every
// published record is: an unsigned fixture would test the refusal, not the path.
func (f *probeFetch) offer(seq uint64, env statelog.Envelope) {
	f.offerStored(seq, probeStoredAt(seq), env)
}

// offerStored is [probeFetch.offer] with the broker instant named: one stored at
// another instant than the record first offered at seq is ANOTHER record.
func (f *probeFetch) offerStored(seq uint64, storedAt time.Time, env statelog.Envelope) {
	body, err := json.Marshal(env)
	if err != nil {
		panic(err)
	}
	body = probeSeal(body)
	f.mu.Lock()
	defer f.mu.Unlock()
	m := statelog.Message{
		Seq:      seq,
		StoredAt: storedAt,
		Payload:  body,
		Ack: func() error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.acked[seq]++
			return nil
		},
		Release: func() error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.released[seq]++
			now, held := f.log[seq]
			if !held {
				return nil
			}
			// ONCE: a record already owed the next fetch is not owed twice.
			for _, owed := range slices.Concat(f.redeliver, f.queue) {
				if owed.Seq == seq {
					return nil
				}
			}
			f.redeliver = append(f.redeliver, now)
			return nil
		},
	}
	f.queue = append(f.queue, m)
	f.log[seq] = m
}

// rewrite replaces what the log holds at seq WITHOUT delivering it: the log a
// node finds after a broker was restored from an older copy and written past
// its rows.
func (f *probeFetch) rewrite(seq uint64, storedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.log[seq]
	m.Seq, m.StoredAt = seq, storedAt
	f.log[seq] = m
}

// endAt makes Bounds report last as the log's end, whatever was offered.
func (f *probeFetch) endAt(last uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.end = &last
}

// Bounds is the lowest and highest sequence ever offered, or the end endAt set.
func (f *probeFetch) Bounds(context.Context) (uint64, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var first, last uint64
	for seq := range f.log {
		if first == 0 || seq < first {
			first = seq
		}
		last = max(last, seq)
	}
	if f.end != nil {
		last = *f.end
	}
	return first, last, nil
}

// At is the record the log holds at seq.
func (f *probeFetch) At(_ context.Context, seq uint64) (string, []byte, time.Time, bool, error) {
	f.mu.Lock()
	m, held := f.log[seq]
	hook := f.atHook
	f.atHook = nil
	f.mu.Unlock()
	if hook != nil {
		// OUTSIDE THE LOCK: a hook offers records, which takes it. This
		// read's answer was taken before it ran.
		hook()
	}
	if !held {
		return "", nil, time.Time{}, false, nil
	}
	return "probe", m.Payload, m.StoredAt, true, nil
}

// Fetch waits out the caller's wait on an empty queue, as a pull consumer
// does: answering nothing at once would spin the apply loop.
func (f *probeFetch) Fetch(ctx context.Context, maxMessages, maxBytes int, wait time.Duration) ([]statelog.Message, error) {
	deadline := time.Now().Add(wait)
	f.mu.Lock()
	f.fetches++
	call := f.fetches
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.answered++
		f.mu.Unlock()
	}()
	for {
		f.mu.Lock()
		if f.failures > 0 {
			f.failures--
			f.mu.Unlock()
			return nil, errors.New("the probe broker did not answer")
		}
		// WHAT WAS HANDED BACK COMES FIRST, ahead of the undelivered.
		if len(f.redeliver) > 0 {
			f.queue, f.redeliver = slices.Concat(f.redeliver, f.queue), nil
		}
		if deliverable := len(f.queue) - f.withhold; deliverable > 0 {
			n := min(deliverable, maxMessages)
			out := f.queue[:n]
			f.queue = f.queue[n:]
			hook := f.after[call]
			f.mu.Unlock()
			if hook != nil {
				// OUTSIDE THE LOCK: a hook offers records,
				// which takes it.
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
	return uint64(len(f.redeliver) + len(f.queue)), nil
}

// releaseCount is how many times the delivery at seq was handed back
// unconsumed.
func (f *probeFetch) releaseCount(seq uint64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.released[seq]
}

// fail makes the broker answer its next n fetches with an error.
func (f *probeFetch) fail(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = n
}

// failing is how many of the failures fail staged are still to come.
func (f *probeFetch) failing() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures
}

// fetchCount is how many times the loop asked.
func (f *probeFetch) fetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches
}

// awaitAnswered blocks until the broker has answered n more of the loop's
// fetches than when called and the loop has STARTED the next one, so it has
// handled every one of those answers. It is how a case lets an idle loop run
// "long enough": on a loaded runner a sleep lets no fetch happen, and an
// assertion that nothing was said during it watched nothing.
//
// The broker answers every fetch within its wait, so only a loop that stopped
// fetching runs the bound out: one wedged between two fetches, or a halted
// runner, which ends the wait at once with its error.
func (f *probeFetch) awaitAnswered(t *testing.T, n int, what string) {
	t.Helper()
	f.mu.Lock()
	start, want, stopped := f.fetches, f.answered+n, f.stopped
	f.mu.Unlock()
	need := want + 1 - start
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		// THE NEXT FETCH STARTED: every answer counted has been handled.
		done := f.answered >= want && f.fetches > want
		made := f.fetches - start
		f.mu.Unlock()
		if done {
			return
		}
		if stopped != nil {
			if err := stopped(); err != nil {
				t.Fatalf("waiting for %s, the runner stopped after the loop made %d of the "+
					"%d fetches it needed: %v", what, made, need, err)
			}
		}
		time.Sleep(time.Millisecond)
	}
	f.mu.Lock()
	made := f.fetches - start
	f.mu.Unlock()
	t.Fatalf("waited 15s for %s: the loop made %d of the %d fetches it needed, so it "+
		"stopped fetching between two", what, made, need)
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
	estate  store.ReplicatedHandle
	runner  *statelog.Runner
	applier *probeApplier
	fetch   *probeFetch
	metrics *metrics.Recorder

	// epoch is the source [applyHarness.rebuild] hands the runner; nil
	// declares none.
	epoch func() map[string]any
}

func newApplyHarness(t *testing.T, domain statelog.Domain) *applyHarness {
	t.Helper()
	return newApplyHarnessWith(t, domain, nil)
}

// newApplyHarnessWith is [newApplyHarness] with the runner's dependencies
// adjusted by mutate before it is built, for a case about one of them.
func newApplyHarnessWith(t *testing.T, domain statelog.Domain,
	mutate func(*statelog.RunnerDeps)) *applyHarness {
	t.Helper()
	return newApplyHarnessOn(t, domain, store.Options{}, mutate)
}

// newApplyHarnessOn is [newApplyHarnessWith] over a store opened with opts, for a
// driver a case wraps to hold or fail the node's commits.
func newApplyHarnessOn(t *testing.T, domain statelog.Domain, opts store.Options,
	mutate func(*statelog.RunnerDeps)) *applyHarness {
	t.Helper()
	dir := t.TempDir()
	db, estate := storetest.OpenEstate(t, filepath.Join(dir, "node.db"), opts, 1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})

	// THE DOMAIN'S OWN TABLES: a real domain ships them in its own
	// migration, and the framework's creates only its own three.
	if err := estate.Tx(t.Context(), func(tx *sql.Tx) error {
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
	deps := statelog.RunnerDeps{
		Domain: domain, Spec: specOf(domain),
		Verifier:   testVerifier(t, domain),
		Applier:    applier,
		Fetch:      fetch,
		Log:        fetch,
		Node:       db,
		DB:         estate,
		Checkpoint: statelog.Position{Generation: 1},
		Metrics:    recorder,
	}
	if mutate != nil {
		mutate(&deps)
	}
	runner, err := statelog.NewRunner(deps)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	fetch.stopped = runner.Stopped
	return &applyHarness{t: t, db: db, estate: estate, runner: runner, applier: applier,
		fetch: fetch, metrics: recorder}
}

// upgrade rebuilds the runner over the SAME database under another domain, as a
// node restarting on a newer build: the applier, fetch and recorder are fresh;
// the rows, checkpoint and retained records survive.
func (h *applyHarness) upgrade(domain statelog.Domain) {
	h.t.Helper()
	h.rebuild(domain, time.Time{})
}

// rebuild is [applyHarness.upgrade] with the stream's creation instant, which
// the loop compares with the one its checkpoint was committed under.
func (h *applyHarness) rebuild(domain statelog.Domain, created time.Time) {
	h.t.Helper()
	probe := newProbeApplier()
	h.rebuildWith(domain, created, probe, probe)
}

// rebuildWith is [applyHarness.rebuild] with applier (the probe, or one wrapping
// it, as [Deriver] does). It is the ONE place a restarting node's runner is
// built, so every variant inherits what a restart is held to, keyring included.
func (h *applyHarness) rebuildWith(domain statelog.Domain, created time.Time,
	applier statelog.Applier, probe *probeApplier) {

	h.t.Helper()
	h.applier = probe
	h.fetch = newProbeFetch()
	recorder, err := metrics.New()
	if err != nil {
		h.t.Fatalf("recorder: %v", err)
	}
	h.metrics = recorder
	// AT THE CHECKPOINT THE ROWS HOLD, as the engine builds a restarting
	// node's runner — generation 1 on a node that never committed.
	checkpoint, found, err := statelog.CheckpointOf(h.t.Context(), h.estate,
		specOf(domain).Name)
	if err != nil {
		h.t.Fatalf("read the checkpoint: %v", err)
	}
	if !found {
		checkpoint.At.Generation = 1
	}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: domain, Spec: specOf(domain),
		Verifier:           testVerifier(h.t, domain),
		Applier:            applier,
		Fetch:              h.fetch,
		Log:                h.fetch,
		Node:               h.db,
		DB:                 h.estate,
		Checkpoint:         checkpoint.At,
		CheckpointStoredAt: checkpoint.StoredAt,
		StreamCreatedAt:    created,
		Epoch:              h.epoch,
		Metrics:            recorder,
	})
	if err != nil {
		h.t.Fatalf("NewRunner: %v", err)
	}
	h.fetch.stopped = runner.Stopped
	h.runner = runner
}

// ledgerHolds reports whether this node's operation ledger holds opID.
func (h *applyHarness) ledgerHolds(opID string) (bool, error) {
	var count int64
	err := h.estate.Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT COUNT(*) FROM probe_ops WHERE op_id = ?`, opID).Scan(&count)
	})
	return count > 0, err
}

// retainedCount is how many records this node still holds that it could not
// decode.
func (h *applyHarness) retainedCount() int64 {
	h.t.Helper()
	var count int64
	if err := h.estate.Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT COUNT(*) FROM probe_log_deferred`).Scan(&count)
	}); err != nil {
		h.t.Fatalf("count the retained records: %v", err)
	}
	return count
}

// boot runs the loop with nothing queued until its STARTUP IS OVER with the
// retained table holding want records, or fails. It is how a reprocess is
// observed: the checkpoint does not move, so [applyHarness.run] has nothing to
// wait on.
//
// Over is the loop's first fetch, never the count alone: the count can read
// want before the loop has run a line, and moves when the last released record
// commits, ahead of its gate counter, Committed hook and the pass's tally. The
// loop fetches only once [statelog.Runner.Run]'s startup has returned, so its
// first fetch is the one instant every startup effect is visible; a run that
// RETURNED has finished its startup too.
func (h *applyHarness) boot(want int64) error {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), 20*time.Second)
	defer cancel()
	// THIS RUN'S fetches, told apart from an earlier run's on the same
	// broker: a case may boot a runner it already ran.
	fetched := h.fetch.fetchCount()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()
	booted := func() bool { return h.fetch.fetchCount() > fetched }
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if booted() && h.retainedCount() == want {
			cancel()
			<-errs
			return nil
		}
		select {
		case err := <-errs:
			// A STOP AT STARTUP is the runner's answer, read through
			// [statelog.Runner.Stopped]; it fails here only if the
			// retained table is not what the case expects.
			if h.retainedCount() == want {
				return nil
			}
			return err
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	<-errs
	if !booted() {
		return fmt.Errorf("the loop never finished its startup: it made no fetch in 10s")
	}
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

// probeDDL is the probe domain's tables. probe_evictions, like the tracker's
// tracker_evictions, is keyed so every append's read of it is one row by key.
const probeDDL = `
CREATE TABLE probe_rows (
    position  INTEGER NOT NULL PRIMARY KEY,
    kind      TEXT    NOT NULL,
    stored_at INTEGER NOT NULL
);
CREATE TABLE probe_evictions (
    node_id             TEXT    NOT NULL,
    log_stream          TEXT    NOT NULL,
    from_position       INTEGER NOT NULL,
    readmitted_position INTEGER,
    PRIMARY KEY (node_id, log_stream)
);
CREATE TABLE probe_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL,
    stored_at  INTEGER NOT NULL DEFAULT 0
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
// Committing the batch and then the cursor relies on redelivering the batch
// after a crash between the two, and a trimmed log has removed the records that
// replay counts on (package doc, "The checkpoint commits with the rows").
func TestTheCheckpointCommitsWithTheRowsItCovers(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	for seq := uint64(1); seq <= 5; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprintf("o%d", seq), fmt.Sprintf("op-%d", seq), 1))
	}
	if err := h.run(5); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Read in one transaction: the contract is that they are simultaneous,
	// not merely present.
	var rows, ops, anchors, cursor int64
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
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
// Retained, because the bytes are the only copy and a rolling upgrade puts
// records on the wire the older half cannot read; the checkpoint advances
// because the log CONSUMED the record, and one that stopped would redeliver it
// for ever (package doc, "A record this build cannot decode is RETAINED, with
// one exception").
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
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
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
	// AND ITS DELIVERY IS ACKNOWLEDGED, or it is redelivered for ever.
	if got := h.fetch.ackCount(2); got != 1 {
		t.Fatalf("the retained record was acknowledged %d time(s), want 1", got)
	}
	if _, held := h.runner.Deferred(); !held {
		t.Error("the runner does not report holding a deferred record")
	}
}

// A LATER RECORD ON STALE ROWS IS RETAINED TOO: the half a per-record decode
// check cannot see. Applied over rows a deferred record never wrote, it makes
// state no other node holds, and both nodes think they are current.
func TestARecordOverAStaleScopeIsRetainedRatherThanApplied(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	// Unreadable, scoped to the CONTAINER of the object the next record
	// edits; nothing on that record's own subject says so.
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
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
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

// AN APPLIER IS TOLD WHAT A COMMITTED BATCH RETAINED, before it is told the
// batch committed, and nothing of the kind about a batch that retained nothing.
//
// A retained record never reaches Apply, yet every read its scope covers became
// unknown at that commit: a dashboard socket held open on a session a retained
// record revoked hears of it only so. Both retain arms count (a record this
// build cannot read, and a readable one in its scope), and the hook comes
// BEFORE Committed because an applier folds it into what Committed hands over.
//
// Mutation: drop the hook call from the runner and the second batch reports
// no retention.
func TestAnApplierIsToldWhatABatchRetained(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, hook := range h.applier.hooksSeen() {
		if hook != "committed" {
			t.Fatalf("a batch that retained nothing called %q", hook)
		}
	}

	// A record above this build, and a readable one inside its scope.
	h.fetch.offer(3, env(3, "edit", "c", "op-3", 9, "project/ENG"))
	h.fetch.offer(4, env(4, "edit", "d", "op-4", 1, "project/ENG/object/d"))
	h.fetch.offer(5, env(5, "edit", "e", "op-5", 1, "project/OPS/object/e"))
	if err := h.run(5); err != nil {
		t.Fatalf("run: %v", err)
	}
	hooks := h.applier.hooksSeen()
	retained := 0
	for i, hook := range hooks {
		var n int
		if _, err := fmt.Sscanf(hook, "retained %d", &n); err != nil {
			continue
		}
		retained += n
		if i+1 >= len(hooks) || hooks[i+1] != "committed" {
			t.Errorf("the hook calls ran %v — Retained must come immediately "+
				"before the Committed of its own batch", hooks)
		}
	}
	if retained != 2 {
		t.Fatalf("the applier was told of %d retained record(s) across %v, want "+
			"2 — the record this build cannot read and the one its scope "+
			"covers", retained, hooks)
	}
}

// A GATE DROPS A DURABLE RECORD, and the anchor and the checkpoint still move.
//
// A gated record applies nowhere, but its position is still the subject's last
// message, so the anchor records it: a writer expecting anything else re-reads
// the number that produced its own rejection until it runs out of rounds.
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
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
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
// Arbitration belongs to the subject KIND, not the stream: the barrier shares
// ONE subject across every read in the company, so a stream-wide rule would
// upsert one hot row per read inside the store's only writer, and a barrier
// would no longer write zero rows or stay out of the applier's budget.
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
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
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

// AN APPLY GATE THIS BUILD CANNOT READ IS A STOP: the one place the retain rule
// inverts.
//
// A deferred gate LICENSES every record above it: an eviction deferred by the
// node it evicts leaves that node's eviction rows empty, the one fence still
// fresh when its coordination path is wedged, so it publishes records every
// peer drops.
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
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
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

// NodeGate answers that same eviction record is a node's.
func (gatingDomain) NodeGate(env statelog.Envelope) bool {
	return env.Kind == "eviction"
}

// A REDELIVERY IS ACKNOWLEDGED AND APPLIED ONCE.
//
// A duplicate in the SAME run is what the committed cursor cannot catch: it is
// above the cursor the transaction started from, and at or below something
// that transaction already applied.
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
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
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
// Its stream keeps one message per subject, so an ordinary write leaves a hole
// a strict loop would stall on for ever, and a positional retention would keep
// records the stream has superseded.
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
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM probe_log_deferred`).Scan(&count); err != nil {
			return err
		}
		// THE SCOPE CHILD IS SUPERSEDED WITH ITS PARENT, found by the
		// parent's position (no foreign key). An orphan reads as a
		// deferral this node cannot clear: the read barrier waits on it
		// and the writer's step 0 refuses to publish.
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
// Not a broadcast per commit that wakes every waiter to re-test its target:
// with a barrier per linearizable read, that is every waiter woken per read.
func TestAWaiterIsWokenOnceByItsOwnCommit(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()

	target := statelog.Position{Stream: probeStream, Generation: 1, Seq: 3}
	woken := make(chan error, 1)
	go func() { woken <- h.runner.WaitReached(ctx, target) }()

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

	// AND A STOPPED APPLIER RELEASES EVERYBODY, or each waits out its
	// budget for a position nothing will reach.
	if got := h.runner.Waiting(); got != 0 {
		t.Fatalf("%d waiter(s) left behind by a stopped applier", got)
	}
}

// THE ACKNOWLEDGEMENT IS BY IDENTITY AND THE TAIL IS CARRIED.
//
// A run holds applied, retained, gated and already-checkpointed records, so
// acknowledging "as many as were applied" acknowledges the WRONG ONES and the
// retained record is redelivered for ever. And when a budget ends a
// transaction early, the rest of the run must be carried, or every
// budget-bounded transaction leaves a hole the loop waits out.
func TestAckIsByIdentityAndTheTailIsCarried(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	// Each record costs most of the row budget, so the transaction ends
	// after the second, at a RECORD boundary.
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
	// AND THE BUDGET BOUND SOMETHING: a case that never reaches the break
	// asserts nothing about the tail.
	if got := len(h.applier.seen()); got != 3 {
		t.Fatalf("the applier saw %d record(s), want 3 — one retained and one "+
			"gated must not reach it", got)
	}
}

// A HOLE IN A STRICT LOG IS WAITED OUT, NOT STEPPED OVER.
//
// A log is contiguous, so a hole is a redelivery in flight. Applying past it
// makes state no other node holds, which the moved checkpoint then hides.
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
	// It must NOT advance to 3 while 2 is missing, across two more fetches
	// answered and handled with the hole open ([probeFetch.awaitAnswered]).
	h.fetch.awaitAnswered(t, 2, "two fetches with the hole open")
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
// The rollback takes the rows, anchor, operation id and checkpoint back
// together: a node can be behind, never inconsistent. And the loop retries the
// run in place: a failure that is not a stop says nothing about whether this
// node can run the company's records, and returning on one leaves the domain
// dead for the life of the process.
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
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
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
	// NOTHING IS ACKNOWLEDGED: acknowledgement follows the commit, so a
	// rollback leaves the deliveries open.
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
// Returning would leave the domain without an applier until the process
// restarts, first seen as a node that lost its seats. A blip is waited out on a
// widening pause and the loop carries on from where it was.
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
// takes a copy out of service.
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
// resumes at the next sequence with no re-sorting, so "two nodes at one
// checkpoint hold the same rows" is about the log's order, not their batching.
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
	// AND IT DID SPLIT, or this asserts nothing about a boundary.
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

// AN EPOCH THAT MOVES REACHES THE APPLIER AT ITS NEXT BATCH, WITH NO RESTART.
//
// The runner is built once, at boot, and the epoch moves with every
// activation. An epoch handed as a VALUE applies every batch under the boot
// company's: a revision reaches the applier only at a restart, and two nodes
// restarted at different times apply one record under two epochs.
//
// ONE RUN spans both records, because a restart between them is exactly what
// the case must not need. Mutation: read the source once, in NewRunner, and
// the second record is handed the first epoch.
func TestAChangedEpochReachesTheApplierWithNoRestart(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	var current atomic.Pointer[map[string]any]
	first := map[string]any{"probe.retention_days": 30}
	current.Store(&first)
	h.epoch = func() map[string]any { return *current.Load() }
	h.rebuild(probeDomain{}, time.Time{})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()
	defer func() {
		cancel()
		<-errs
	}()
	committed := func(want uint64) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for h.runner.Committed().Seq < want {
			if err := h.runner.Stopped(); err != nil {
				t.Fatalf("the runner stopped: %v", err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("the runner committed %v, want sequence %d",
					h.runner.Committed(), want)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	h.fetch.offer(1, env(1, "edit", "o1", "op-1", 1))
	committed(1)
	second := map[string]any{"probe.retention_days": 7}
	current.Store(&second)
	h.fetch.offer(2, env(2, "edit", "o2", "op-2", 1))
	committed(2)

	seen := h.applier.epochsSeen()
	if len(seen) != 2 {
		t.Fatalf("the applier was handed %d epoch(s), want one per record", len(seen))
	}
	if seen[0]["probe.retention_days"] != 30 {
		t.Errorf("the first record was applied under %v, want the epoch in force "+
			"when it arrived", seen[0])
	}
	if seen[1]["probe.retention_days"] != 7 {
		t.Errorf("the second record was applied under %v after the epoch moved "+
			"to %v — a revision reaches this runner only at a restart", seen[1], second)
	}
}

// THE OPERATION LEDGER IS SWEPT.
//
// It holds a row per applied record on every data node, and its only reader
// asks "did my operation land", so an unswept ledger stays correct while the
// file grows. The horizon is the CLIENT'S: a seat re-asks about an op id on its
// next wake, hours or a weekend later, and an op id that outlived its row
// resolves `unknown`, not `applied`, sending a turn to re-decide done work.
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
	if held, err := h.ledgerHolds("op-3"); err != nil || !held {
		t.Fatalf("op-3 is not in the ledger after applying it (held=%v, %v)",
			held, err)
	}

	// A CUTOFF IN THE FUTURE sweeps everything: under test is that the
	// delete happens and reports what it did.
	swept, err := h.runner.PurgeOps(t.Context(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("PurgeOps: %v", err)
	}
	if swept != 6 {
		t.Errorf("the sweep deleted %d of 6 operation rows", swept)
	}
	if held, err := h.ledgerHolds("op-3"); err != nil || held {
		t.Errorf("op-3 survived a sweep past its own instant (held=%v, %v)",
			held, err)
	}

	// AND THE CONTROL: a row inside the horizon stays, or the sweep is a
	// truncate.
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
	if held, err := h.ledgerHolds("op-7"); err != nil || !held {
		t.Errorf("op-7 was swept inside its own retention (held=%v, %v)",
			held, err)
	}
}

// A BACKLOG WIDER THAN ONE BATCH STILL DRAINS.
//
// The sweep is batched because it shares the file with the applier's commits,
// and one statement over a month of rows holds the writer throughout. A loop
// that stopped after one batch looks identical on a small table and leaves the
// month behind on a node returning from a long absence.
func TestTheOperationSweepDrainsABacklogWiderThanOneBatch(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	// Seeded directly: applying that many records is a minute of round
	// trips.
	rows := statelog.OpsPurgeBatch + 7
	if err := h.estate.Tx(t.Context(), func(tx *sql.Tx) error {
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
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
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
// `apply.tx.aborts` says whether an apply's body ever runs twice, and reads
// zero by construction: the store's write transactions hold the file's lock
// from BEGIN, so nothing elsewhere can abort one. Counting every run would read
// the retry reserve as spent on a healthy fleet, one off-by-one away: a first
// attempt is not a retry.
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
	// THE CONTROL: the assertion above passes too with nothing wired.
	if got := h.counter(metrics.StatelogApplyRecords); got == 0 {
		t.Error("the applier recorded no records at all, so this recorder is " +
			"not connected and the assertion above proves nothing")
	}
}

// A RE-RUN APPLY TRANSACTION SAYS WHAT IT SAW ONCE.
//
// The store re-runs an apply's body on a transient failure; its rows roll back,
// but a counter bumped or a line logged inside it does not, and the alarm table
// reads `records.unverifiable` and `records.gated`. The LAST record of the run
// fails its first apply as a lost lock does, so the whole body, the
// unverifiable and the gated record included, runs twice.
func TestARerunApplySaysWhatItSawOnce(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.applier.gate = statelog.ReasonEvicted
	h.applier.gated[2] = true
	h.applier.lockedAt = 3
	h.fetch.offerFramed(1, sealedUnder(t, "k9", env(1, "edit", "a", "op-1", 1)))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	h.fetch.offer(3, env(3, "edit", "c", "op-3", 1))
	if err := h.run(3); err != nil {
		t.Fatalf("run: %v", err)
	}
	// THE CONTROL: the body did run twice, or nothing below is about a
	// re-run at all.
	if got := h.counter(metrics.StatelogApplyTxAborts); got != 1 {
		t.Fatalf("the store re-ran the apply body %d time(s), want exactly 1 "+
			"— without the re-run this case asserts nothing", got)
	}
	if got := h.retainedCount(); got != 1 {
		t.Fatalf("retained %d, want the one record this node cannot "+
			"authenticate", got)
	}
	if got := h.counter(metrics.StatelogRecordsUnverifiable); got != 1 {
		t.Errorf("one unverifiable record was counted %d times — the count "+
			"is taken inside a body the store ran twice", got)
	}
	if got := h.counter(metrics.StatelogRecordsGated); got != 1 {
		t.Errorf("one gated record was counted %d times — the count is "+
			"taken inside a body the store ran twice", got)
	}
}

// A RECORD A GATE DROPPED IS COUNTED ONCE, WHEN ITS DROP COMMITS — however
// many times the store runs the transaction that drops it.
//
// A re-run body asks the gate again, so counted where the gate is asked, one
// dropped record counts once per attempt on the counter the records_gated
// alarm reads.
func TestAGatedRecordIsCountedOnceHoweverOftenItsTransactionRuns(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.applier.gate, h.applier.gated[1] = statelog.ReasonEvicted, true
	// THE SAME TRANSACTION, which fails at the record after the dropped one
	// and is run again from its start.
	h.applier.lockedAt = 2
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}
	// THE CONTROL: the body ran twice over the dropped record; a batch split
	// between the two would pass below having re-run nothing.
	h.applier.mu.Lock()
	asked := h.applier.asked[1]
	h.applier.mu.Unlock()
	if asked != 2 {
		t.Fatalf("the gate was asked about the record at 1 %d time(s), want 2 — "+
			"the transaction that dropped it was not run again, so this proves "+
			"nothing", asked)
	}
	if got := h.counter(metrics.StatelogApplyTxAborts); got != 1 {
		t.Fatalf("the applier counted %d aborted transaction(s), want the one the "+
			"lock timeout at 2 cost", got)
	}
	if n := gatedUnder(h, statelog.ReasonEvicted); n != 1 {
		t.Errorf("one record dropped by the eviction gate was counted %d time(s) "+
			"under %q — once per attempt at the transaction rather than once for "+
			"the drop it committed", n, statelog.ReasonEvicted)
	}
	if n := appliedAs(h, "gated"); n != 1 {
		t.Errorf("the batch counted %d record(s) consumed as gated, want 1", n)
	}
}

// A RETAINED RECORD A GATE DROPS WHEN IT IS REPROCESSED IS GATED, NOT
// REPROCESSED — counted, logged and released exactly as the live loop drops it.
//
// The retaining build never asked a gate about the record; the build that reads
// it asks at its next boot, and the gate may drop it (writer evicted since,
// generation voided by a reanchor, object deleted). That drop writes no row, so
// a tally of it as reprocessed reports rows this node lacks, and the
// records-gated counter is a dropped record's only witness. What it held back
// by scope is released with it: nothing applied is left to order it behind.
func TestAReprocessedRecordAGateDropsIsCountedGated(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	// ABOVE THE RETAINING BUILD, so it is retained without a gate being asked.
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 9))
	// ON THE ROWS 2 WOULD HAVE MADE STALE, so retained behind it.
	h.fetch.offer(3, env(3, "edit", "b", "op-3", 1))
	if err := h.run(3); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.retainedCount(); got != 2 {
		t.Fatalf("the retaining build kept %d record(s), want 2", got)
	}

	// THE UPGRADE: a build that reads the record, and whose domain gate drops
	// it — its writer has been evicted since it was retained.
	h.upgrade(upgradedDomain{reads: 9})
	h.applier.mu.Lock()
	h.applier.gate, h.applier.gated[2] = statelog.ReasonEvicted, true
	h.applier.mu.Unlock()
	if err := h.boot(0); err != nil {
		t.Fatalf("the upgraded build's boot: %v", err)
	}
	seen := h.applier.seen()
	if len(seen) != 1 || seen[0].Seq != 3 {
		t.Fatalf("the upgraded build applied %v, want only the record at 3 — the "+
			"record at 2 is dropped by the eviction gate, and 3 is released "+
			"from behind it", seen)
	}
	rows := probeRows(t, h)
	want := []int64{
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}.Packed(),
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 3}.Packed(),
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("the node holds rows at %v, want %v", rows, want)
	}
	if n := gatedUnder(h, statelog.ReasonEvicted); n != 1 {
		t.Errorf("the upgraded build counted %d record(s) gated %q, want 1 — a "+
			"dropped record's only witness is this counter", n, statelog.ReasonEvicted)
	}
	for op, applied := range map[string]bool{"op-2": false, "op-3": true} {
		held, err := h.ledgerHolds(op)
		if err != nil {
			t.Fatalf("read the ledger: %v", err)
		}
		if held != applied {
			t.Errorf("the ledger holds %s: %v, want %v", op, held, applied)
		}
	}
	// AND COUNTED AS THE LIVE LOOP COUNTS IT: a record a gate dropped wrote no
	// row, so it is `gated` and not `reprocessed`.
	for result, want := range map[string]uint64{"reprocessed": 1, "gated": 1} {
		if got := appliedAs(h, result); got != want {
			t.Errorf("the upgraded build counted %d record(s) %q, want %d", got,
				result, want)
		}
	}
}

// probeRows is every position the probe applier has written a row at, in
// order.
func probeRows(t *testing.T, h *applyHarness) []int64 {
	t.Helper()
	var out []int64
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `SELECT position FROM probe_rows ORDER BY position`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var at int64
			if err := rows.Scan(&at); err != nil {
				return err
			}
			out = append(out, at)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the probe rows: %v", err)
	}
	return out
}

// TestTheApplierMeasuresItsOwnDrain: the drain is what three answers divide a
// backlog by to state a TIME (a bounded stale read's staleness, a refusal's
// `retry_after_seconds`, the apply-lag alarm). Unmeasured, all three fall back
// to one record per second, so a node a second of work behind reports half an
// hour, refuses reads it should serve and fires an alarm nobody can act on.
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
	// A SANITY BOUND, not a threshold: the figure is a RATE, not a count, a
	// constant or the one-per-second fallback.
	if drain < 1 {
		t.Fatalf("the drain is %v records/second over twenty records applied in "+
			"a test — that is the unmeasured fallback rather than a "+
			"measurement", drain)
	}

	// AND THE COMMIT RATE BESIDE IT: rows per second is progress, commits
	// per second the FSYNC rate a device's write budget is spent by. They
	// move independently (a run fills toward the transaction budget), so
	// a doubled commit rate at a healthy row rate is twice the disk work
	// for the same progress, which neither figure alone shows.
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
// The broker reports records pending until it DELIVERS them, including those
// it withholds at the consumer's in-flight ceiling; the run's own records fill
// that ceiling and are acknowledged only after it commits. So a loop that
// pulled while anything was pending would wait on its own commit for ever. The
// linger bounds it: a pull that returns nothing inside it closes the run.
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
	// THE ACKNOWLEDGEMENTS FOLLOW THE COMMIT (the checkpoint is the
	// authority), so they land a moment after the checkpoint moves: waited
	// for, then counted exactly.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (h.fetch.ackCount(1) == 0 || h.fetch.ackCount(2) == 0) {
		time.Sleep(2 * time.Millisecond)
	}
	if one, two := h.fetch.ackCount(1), h.fetch.ackCount(2); one != 1 || two != 1 {
		cancel()
		<-errs
		t.Fatalf("the committed records were acknowledged %d and %d time(s), "+
			"want once each", one, two)
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
// Otherwise a node upgraded through a rolling upgrade keeps its deferrals,
// refusing every read and write about the objects they cover, for ever, and
// naming a record version it already runs. The record retained BECAUSE its
// scope met the undecodable one applies too, after it: it was decodable all
// along, and what kept it back was order.
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
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
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

// A RECREATED STREAM STOPS THE APPLIER, on the stream's own creation instant.
//
// A remade stream restarts its sequences at one, so every position this node
// holds names a number space that no longer exists, and a consumer resumed from
// the checkpoint looks caught up while applying none of the new records. Only
// the instant can notice: the sequences are plausible, and an empty stream and
// an emptied one have the same count. It must be the LIVE instant: the
// checkpoint row's own value compared with itself never fires.
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

	// THE SAME STREAM at nanosecond precision, against a row that keeps
	// microseconds, must read as the same.
	h.rebuild(probeDomain{}, born.Add(100*time.Nanosecond))
	if err := h.boot(0); err != nil {
		t.Fatalf("the same stream, reported at a finer resolution, was refused: %v", err)
	}
	if err := h.runner.Stopped(); err != nil {
		t.Fatalf("the same stream stopped the applier: %v", err)
	}
	if err := h.runner.StreamIdentity(); err != nil {
		t.Fatalf("the same stream reads as a foreign one: %v", err)
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
	// AND THE PUBLISHER KNOWS: the write path asks the identity, never the
	// loop's error, so the identity carries the finding or writes go on
	// landing on a log this node's rows are not keyed to.
	if identity := h.runner.StreamIdentity(); !errors.Is(identity, statelog.ErrStreamRecreated) {
		t.Fatalf("StreamIdentity after the boot found a recreated stream = %v, "+
			"want %v — the writes would not refuse", identity, statelog.ErrStreamRecreated)
	}
	if !strings.Contains(err.Error(), "reanchor") {
		t.Fatalf("the stop does not name the verb that repairs it: %v", err)
	}
	if got := h.runner.Committed().Seq; got != 2 {
		t.Fatalf("the checkpoint moved to %d on a stopped applier", got)
	}
}

// A LIVE READING OF A REBUILT LOG IS THE RUNNER'S VERDICT, and it outlives the
// loop that was running when it was taken.
//
// The boot compares the checkpoint once; a stream rebuilt under a running node
// shows only in a later reading, and every one is handed here, so the reads and
// the writes share one answer. A reading of the SAME stream at the broker's
// finer resolution, or one that could not be taken, establishes nothing; and a
// re-run does not clear an established rebuild: unlike a stop, about rows an
// adoption may replace, it is about the positions the runner was built on.
func TestALiveReadingOfARebuiltLogIsTheRunnersVerdict(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	born := time.Date(2026, 9, 10, 12, 0, 0, 123_456_789, time.UTC)
	h.rebuild(probeDomain{}, born)
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	if err := h.run(1); err != nil {
		t.Fatalf("run: %v", err)
	}

	for name, live := range map[string]time.Time{
		"the same stream at a finer resolution": born.Add(100 * time.Nanosecond),
		"a reading that could not be taken":     {},
	} {
		if h.runner.ObserveStream(live) {
			t.Fatalf("%s established a rebuild", name)
		}
		if err := h.runner.StreamIdentity(); err != nil {
			t.Fatalf("after %s the identity is %v, want the live stream — a "+
				"node that read its own stream as foreign would refuse every "+
				"write it makes", name, err)
		}
	}

	rebuiltAt := born.Add(time.Hour)
	if !h.runner.ObserveStream(rebuiltAt) {
		t.Fatal("a reading of a stream created an hour later did not establish " +
			"a rebuild")
	}
	identity := h.runner.StreamIdentity()
	if !errors.Is(identity, statelog.ErrStreamRecreated) {
		t.Fatalf("StreamIdentity = %v, want %v", identity, statelog.ErrStreamRecreated)
	}
	for _, want := range []string{
		born.Format(time.RFC3339Nano), rebuiltAt.Format(time.RFC3339Nano), "reanchor",
	} {
		if !strings.Contains(identity.Error(), want) {
			t.Errorf("the refusal %q does not name %q — an operator needs both "+
				"instants and the verb that follows the new stream", identity, want)
		}
	}

	// ESTABLISHED ONCE, so the line naming the rebuild is written once,
	// not per beat.
	if h.runner.ObserveStream(rebuiltAt) {
		t.Fatal("a second reading of the same rebuild reported it again")
	}
	// AND A RE-RUN NEITHER FORGETS IT NOR APPLIES FROM THE REBUILT STREAM:
	// the checkpoint row still names the instant the runner was built with,
	// so the boot comparison passes, and a resumed loop would apply the new
	// stream's records into rows keyed to the old. It stops on the verdict.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := h.runner.Run(ctx); !errors.Is(err, statelog.ErrStopped) ||
		!errors.Is(err, statelog.ErrStreamRecreated) {
		t.Fatalf("a re-run over an established rebuild returned %v, want the "+
			"recreation stop", err)
	}
	if !errors.Is(h.runner.StreamIdentity(), statelog.ErrStreamRecreated) {
		t.Fatal("a re-run cleared the rebuild, so the writes would start " +
			"landing on it again")
	}
	if stopped := h.runner.Stopped(); !errors.Is(stopped, statelog.ErrStreamRecreated) {
		t.Fatalf("a re-run over an established rebuild reports %v, want the "+
			"recreation stop", stopped)
	}
}

// A CHECKPOINT PAST THE LOG'S END IS THE RUNNER'S VERDICT WHILE THE END STAYS
// BELOW IT. Only that shows a broker restored from an older copy (it keeps its
// creation instant), and fence 0 refuses every write on the verdict:
//
//   - a reading AT the end (caught up) or past it (busy) establishes nothing;
//   - one paired with the zero checkpoint a boot's first heartbeat reads,
//     before the loop loads its row, does not clear it;
//   - a re-run over the same checkpoint keeps it;
//   - an end reaching the checkpoint clears it, so one stale member's answer
//     cannot stop a healthy node's writes for good;
//   - a checkpoint the runner left (an adopted snapshot) drops it.
func TestACheckpointPastTheEndIsTheRunnersVerdictWhileTheEndStaysBelow(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	for seq := uint64(1); seq <= 3; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprint(seq), fmt.Sprintf("op-%d", seq), 1))
	}
	if err := h.run(3); err != nil {
		t.Fatalf("run: %v", err)
	}
	at := h.runner.Stance()
	unloaded := statelog.StanceAt(statelog.Position{Stream: at.At.Stream, Generation: at.At.Generation})

	for name, last := range map[string]uint64{
		"a reading at the end": 3,
		"a reading past it":    10,
	} {
		if established, reached := h.runner.ObserveEnd(at, last); established || reached {
			t.Fatalf("%s changed the verdict (established %v, reached %v)",
				name, established, reached)
		}
		if err := h.runner.StreamIdentity(); err != nil {
			t.Fatalf("after %s the identity is %v — a caught-up node would "+
				"refuse every write it makes", name, err)
		}
	}

	if established, _ := h.runner.ObserveEnd(at, 2); !established {
		t.Fatal("a log ending at 2 under a checkpoint of 3 did not establish " +
			"anything, so fence 0 would let every ordinary write through")
	}
	identity := h.runner.StreamIdentity()
	if !errors.Is(identity, statelog.ErrAheadOfLog) {
		t.Fatalf("StreamIdentity = %v, want %v", identity, statelog.ErrAheadOfLog)
	}
	if errors.Is(identity, statelog.ErrStreamRecreated) {
		t.Fatalf("StreamIdentity = %v names a rebuild — a broker restored from "+
			"an older copy kept its stream, and naming a rebuild sends an "+
			"operator looking for a delete that never happened", identity)
	}
	for _, want := range []string{"at sequence 3", "ends at 2", "reanchor"} {
		if !strings.Contains(identity.Error(), want) {
			t.Errorf("the refusal %q does not name %q", identity, want)
		}
	}
	if established, _ := h.runner.ObserveEnd(at, 2); established {
		t.Fatal("a second reading of the same state reported it again, so the " +
			"line naming it would be written on every beat")
	}

	if established, reached := h.runner.ObserveEnd(unloaded, 2); established || reached {
		t.Fatalf("a reading paired with an unloaded checkpoint changed the verdict "+
			"(established %v, reached %v)", established, reached)
	}
	if !errors.Is(h.runner.StreamIdentity(), statelog.ErrAheadOfLog) {
		t.Fatal("a reading paired with the zero a runner reports before it loads " +
			"its row cleared the verdict — the first heartbeat of every boot " +
			"would reopen the writes a restored broker must refuse")
	}

	// A RE-RUN OVER THE SAME CHECKPOINT keeps it; its record only proves
	// the loop loaded the row.
	h.fetch.offer(4, env(4, "edit", "4", "op-4", 1))
	if err := h.run(4); err != nil {
		t.Fatalf("run again: %v", err)
	}
	if !errors.Is(h.runner.StreamIdentity(), statelog.ErrAheadOfLog) {
		t.Fatal("a re-run over the same checkpoint cleared the verdict")
	}

	// THE END REACHING THE CHECKPOINT the verdict names clears it, once.
	if _, reached := h.runner.ObserveEnd(unloaded, 3); !reached {
		t.Fatal("an end that reached the checkpoint did not clear the verdict — " +
			"one stale member's answer would stop this node's writes for good")
	}
	if err := h.runner.StreamIdentity(); err != nil {
		t.Fatalf("after the end reached the checkpoint the identity is %v", err)
	}
	if _, reached := h.runner.ObserveEnd(unloaded, 3); reached {
		t.Fatal("the end reaching the checkpoint was reported twice")
	}

	// A CHECKPOINT THIS RUNNER NO LONGER STANDS AT drops the verdict: the
	// row is rewritten, as an adoption leaves it, to a donor's position
	// below the log's end naming the log's own record there.
	if established, _ := h.runner.ObserveEnd(h.runner.Stance(), 2); !established {
		t.Fatal("the verdict was not re-established for the adoption case")
	}
	if err := h.estate.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE statelog_cursor SET seq = 1, stored_at = ? WHERE stream = ?`,
			store.EncodeTime(probeStoredAt(1)), probeStream)
		return err
	}); err != nil {
		t.Fatalf("rewrite the checkpoint: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()
	for h.runner.Committed().Seq != 1 && ctx.Err() == nil {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-errs
	if got := h.runner.Committed().Seq; got != 1 {
		t.Fatalf("the loop loaded checkpoint %d, want the adopted 1", got)
	}
	if err := h.runner.StreamIdentity(); err != nil {
		t.Fatalf("after adopting a checkpoint below the log's end the identity is "+
			"%v — a verdict about a position the runner left would refuse every "+
			"write until the log happened to reach it", err)
	}
}

// A REBUILD OUTRANKS A CHECKPOINT PAST THE END: it is permanent where the other
// clears, and it is the finding an operator has to act on first.
func TestARebuildOutranksACheckpointPastTheEnd(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	born := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	h.rebuild(probeDomain{}, born)
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	if err := h.run(1); err != nil {
		t.Fatalf("run: %v", err)
	}
	h.runner.ObserveEnd(h.runner.Stance(), 0)
	h.runner.ObserveStream(born.Add(time.Hour))
	identity := h.runner.StreamIdentity()
	if !errors.Is(identity, statelog.ErrStreamRecreated) || errors.Is(identity, statelog.ErrAheadOfLog) {
		t.Fatalf("StreamIdentity = %v, want the rebuild alone", identity)
	}
}

// A RUNNER RUNS AGAIN FROM THE CHECKPOINT IT NOW HOLDS.
//
// An adoption ends every apply loop, replaces the file and restarts the loops
// on the same runners, which every subsystem holds. So Run is re-enterable: it
// resumes from the file's checkpoint, and the last run's stop is about rows
// this node no longer has.
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
		waited <- h.runner.WaitReached(ctx, statelog.Position{Stream: probeStream, Generation: 1, Seq: 3})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && h.runner.Waiting() == 0 {
		time.Sleep(time.Millisecond)
	}
	runCtx, stopRun := context.WithCancel(t.Context())
	errs := make(chan error, 1)
	// STOPPED ONCE IT IS RUNNING: a loop fetches only after its startup,
	// so its first fetch marks the loop the waiter waits on.
	fetched := h.fetch.fetchCount()
	go func() { errs <- h.runner.Run(runCtx) }()
	for h.fetch.fetchCount() <= fetched {
		select {
		case err := <-errs:
			t.Fatalf("the second run ended before it fetched: %v", err)
		case <-ctx.Done():
			t.Fatal("the second run never fetched")
		case <-time.After(time.Millisecond):
		}
	}
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
// [reorderBuffer.admit] hands a record below the run's high-water mark straight
// through, since an unacknowledged redelivery comes back for ever, so a run can
// close as [1, 2, 1]: the checkpoint is the HIGHEST position applied, never the
// tail. A tail checkpoint understates its own rows: a linearizable read waiting
// for 2 is refused `behind`, and the next boot re-applies a record whose anchor
// it already passed.
func TestALateRedeliveryDoesNotRegressTheCheckpoint(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "object", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "object", "b", "op-2", 1))
	// THE ACKNOWLEDGEMENT OF 1 WAS LOST, so the broker hands it back while
	// the loop holds 2, in a later fetch: one batch is deduped on arrival.
	h.fetch.afterFetch(1, func() {
		h.fetch.offer(1, env(1, "object", "a", "op-1", 1))
	})

	ctx, cancel := context.WithTimeout(h.t.Context(), 20*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()

	// THE ACK SIGNALS THE COMMIT whatever the checkpoint says, so a broken
	// checkpoint fails promptly rather than at a timeout.
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
	at, _, found, err := statelog.CursorFor(h.t.Context(), h.estate,
		specOf(probeDomain{}).Name)
	if err != nil {
		t.Fatalf("read the checkpoint: %v", err)
	}
	if !found || at.Seq != 2 {
		t.Errorf("the checkpoint row is at %s (found=%v), want sequence 2 — a "+
			"cursor below rows its own transaction committed is the one state "+
			"the checkpoint exists to rule out", at, found)
	}
	// AND THE REDELIVERY IS STILL ACKNOWLEDGED, which is why admit passes
	// it through: dropped, it would be redelivered for ever.
	if got := h.fetch.ackCount(1); got != 2 {
		t.Errorf("sequence 1 was acknowledged %d time(s), want 2 — the "+
			"original delivery and the redelivery", got)
	}
}

// flakyEstate is a replicated estate whose connection cannot be pinned for the
// first `refusals` attempts: a store momentarily unavailable, as an adoption's
// close-and-reopen or a refused transaction looks to the applier.
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
// Pinning the connection, reading the checkpoint and reprocessing retained
// records are database work inside the retry loop: returning out of Run on a
// momentary refusal (an adoption's bracket, a refused transaction, a slow disk)
// leaves the domain with no applier for the life of the process, with Stopped
// nil and no fault, so the node goes on publishing a caught-up position.
func TestAStoreThatRefusesAtStartupIsRetried(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	flaky := &flakyEstate{inner: h.estate, refusals: 3}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: probeDomain{}, Spec: specOf(probeDomain{}),
		Verifier:   testVerifier(t, probeDomain{}),
		Applier:    h.applier,
		Fetch:      h.fetch,
		Log:        h.fetch,
		Node:       h.db,
		DB:         flaky,
		Checkpoint: statelog.Position{Generation: 1},
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

// probeSeal signs a fixture's record under the keyring the harness verifies
// with, panicking: a fixture that cannot sign is a broken test file, not a case.
func probeSeal(body []byte) []byte {
	signer, err := recordseal.NewSigner(probeDomain{}.Name(), testRing())
	if err != nil {
		panic(err)
	}
	return signer.Seal(body)
}

// ONE SUBJECT KIND'S LEDGER ROWS CAN BE SWEPT ON THEIR OWN HORIZON — and only
// that kind's.
//
// A kind no slow client re-asks about declares a shorter horizon (the identity
// estate's sessions: an hour against the month), swept as a RANGE over the
// stored subject. Each row is one way a range goes wrong: the kind's old rows
// and its row with no id go; its recent row stays, as do every other kind and
// one whose name merely BEGINS with this one's (the byte after the name is
// what a naive prefix test gets wrong).
func TestOneSubjectKindsLedgerRowsAreSweptOnTheirOwn(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	now := time.Now()
	old, recent := now.Add(-2*time.Hour), now
	rows := []struct {
		op, subject string
		at          time.Time
		swept       bool
	}{
		{"session-old", probePrefix + ".session.l1", old, true},
		{"session-bare", probePrefix + ".session", old, true},
		{"session-recent", probePrefix + ".session.l2", recent, false},
		{"lookalike-old", probePrefix + ".sessions.l3", old, false},
		{"other-old", probePrefix + ".object.o1", old, false},
	}
	if err := h.estate.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, r := range rows {
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO probe_ops (op_id, subject, position, applied_at)
				VALUES (?, ?, 1, ?)`, r.op, r.subject, store.EncodeTime(r.at)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the ledger: %v", err)
	}

	swept, err := h.runner.PurgeOpsOfKind(t.Context(), "session", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("PurgeOpsOfKind: %v", err)
	}
	want := 0
	for _, r := range rows {
		_, held, err := h.runner.Op(t.Context(), r.op)
		if err != nil {
			t.Fatalf("Op(%s): %v", r.op, err)
		}
		if held == r.swept {
			t.Errorf("%s (%s): held=%v after the session sweep, want swept=%v",
				r.op, r.subject, held, r.swept)
		}
		if r.swept {
			want++
		}
	}
	if swept != int64(want) {
		t.Errorf("the sweep reported %d row(s) and deleted %d", swept, want)
	}
}

// THE DRAIN IS THE APPLIER'S OWN OBSERVATION, AND IT IS A HISTORY.
//
// [statelog.Health.Drained] answers "have this node's rows ever been the whole
// state", which no single lag reading can: derived per heartbeat from
// `lag == 0`, one unapplied record makes it false for a beat, and a solo node
// releases its seats on every burst of writes. The loop is the only witness
// that cannot miss a drain: an empty fetch while it holds nothing IS one.
func TestTheDrainLatchIsTheApplierSOwnObservation(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	if h.runner.Drained() {
		t.Fatal("a loop that has never run reports its rows a whole state, so a " +
			"node vouches for a copy it has not read a record of")
	}
	for seq := uint64(1); seq <= 2; seq++ {
		h.fetch.offer(seq, env(seq, "edit", string(rune('a'+seq-1)),
			fmt.Sprintf("op-%d", seq), 1))
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()
	waitForDrain(t, h, true, "the loop to reach the end of its log")

	// AND IT MEANS LEVEL, not merely "the loop ran": the records are
	// applied and committed before the fetch that finds nothing behind
	// them.
	if got := h.runner.Committed().Seq; got != 2 {
		cancel()
		<-errs
		t.Fatalf("the loop reported itself drained at checkpoint %d, want 2 — it "+
			"is vouching for rows it has not written", got)
	}
	cancel()
	<-errs
	if !h.runner.Drained() {
		t.Error("a stopped loop forgot that it had drained; the answer a reader " +
			"gets after a shutdown is the last honest one, not a fresh doubt")
	}

	// A RESTART CLEARS IT: re-entering Run is what an adoption does,
	// installing a peer's rows under a history this loop never applied.
	// The broker refuses the first fetches, and a loop that cannot reach
	// the log has not reached its end, so the answer stays false.
	h.fetch.mu.Lock()
	h.fetch.failures = 4
	h.fetch.mu.Unlock()
	before := h.fetch.fetchCount()
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	go func() { errs <- h.runner.Run(ctx) }()
	waitForDrain(t, h, false, "the restarted loop to drop its drain latch")

	// AND IT COMES BACK ONLY WHEN THE BROKER ANSWERS ONE. Counted from the
	// restart: fewer than five fetches means the latch was set while every
	// one was failing.
	waitForDrain(t, h, true, "the loop to drain once the broker answers again")
	if got := h.fetch.fetchCount() - before; got <= 4 {
		t.Errorf("the restarted loop latched after %d fetch(es), and the first "+
			"4 were refused — the latch is being set by something other than "+
			"an answer from the broker", got)
	}
	cancel()
	<-errs
}

// A LOG BUSIER THAN A FETCH WAIT STILL DRAINS: a run the broker had nothing
// left behind, committed whole, is as much the end of the log as an empty
// fetch.
//
// On a log gaining a record more often than [statelog.FetchWait], every fetch
// returns one, so a latch set only on an empty fetch leaves a restarted data
// node a LAGGING copy (asked last, never a snapshot's donor) until the company
// goes quiet. Here records arrive five to a fetch wait.
func TestALogBusierThanAFetchWaitStillDrains(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	const records = 60
	every := statelog.FetchWait / 5
	stop, feeding := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(feeding)
		for seq := uint64(1); seq <= records; seq++ {
			select {
			case <-stop:
				return
			case <-time.After(every):
			}
			h.fetch.offer(seq, env(seq, "edit", fmt.Sprintf("o%d", seq), fmt.Sprintf("op-%d", seq), 1))
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()
	defer func() {
		close(stop)
		<-feeding
		cancel()
		<-errs
	}()
	for !h.runner.Drained() {
		select {
		case <-feeding:
			t.Fatalf("the loop said it drained only once the records stopped coming, %v apart: "+
				"a busy log's copy never drains", every)
		case <-time.After(2 * time.Millisecond):
		}
	}
	if h.runner.Committed().Seq == 0 {
		t.Fatal("the loop said it drained before it had applied a record the log held")
	}
}

// A LOOP HOLDING A RECORD ABOVE A HOLE HAS NOT DRAINED, however empty the
// broker's answers: its rows are a prefix of the log, short of the record its
// reorder buffer holds for the one below.
func TestALoopHoldingARecordAboveAHoleHasNotDrained(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(3, env(3, "edit", "c", "op-3", 1)) // above a hole at 2
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()
	defer func() {
		cancel()
		<-errs
	}()
	// EMPTY FETCHES, several: each is a FetchWait the broker answered with
	// nothing while 3 sat in the buffer.
	for start := time.Now(); time.Since(start) < 3*statelog.FetchWait; time.Sleep(2 * time.Millisecond) {
		if h.runner.Drained() {
			t.Fatalf("the loop said it drained at checkpoint %d, holding 3 above the hole at 2",
				h.runner.Committed().Seq)
		}
	}
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	waitForDrain(t, h, true, "the loop to drain once the hole closed")
	if got := h.runner.Committed().Seq; got != 3 {
		t.Fatalf("the loop said it drained at checkpoint %d, want 3", got)
	}
}

// THE FIRST DRAIN OF A RUN IS TOLD, ONCE, AND ONLY ONCE THE LATCH READS IT.
//
// A node's snapshot gate declines until every applier has drained, and one
// that only re-asks learns of it a boot's settling time late. So the loop tells
// it when [statelog.Runner.Drained] turns true: never before (a gate woken
// early reads false and declines), never for the run's later drains (every
// empty fetch is one, waking the gate once a fetch interval), and again after
// the loop runs anew, whose drain is about a different history.
func TestTheFirstDrainOfARunIsToldOnce(t *testing.T) {
	t.Parallel()
	var h *applyHarness
	told := make(chan bool, 16)
	h = newApplyHarnessWith(t, probeDomain{}, func(d *statelog.RunnerDeps) {
		d.OnDrained = func() { told <- h.runner.Drained() }
	})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()
	awaitTold(t, told, "the first drain")
	// EVERY LATER EMPTY FETCH IS A DRAIN TOO, and says nothing: three more
	// answered empty and handled.
	h.fetch.awaitAnswered(t, 3, "three idle fetches after the first drain")
	select {
	case <-told:
		t.Fatal("a drain after the first of the run was told again")
	default:
	}
	cancel()
	<-errs

	// A RUN THAT STARTS AGAIN drains a history of its own, and says so.
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	go func() { errs <- h.runner.Run(ctx) }()
	awaitTold(t, told, "the restarted loop's first drain")
	cancel()
	<-errs
}

// THE LOOP SAYS SO EACH TIME IT COMES LEVEL WITH ITS LOG, AND ONLY THEN.
//
// A node's seat claims ask whether it is level NOW: after a boot's first
// drain, and after every burst of writes and every outage retried across. Each
// is told after its records commit, so a woken gate reads the rows it was woken
// for; an idle loop, every fetch empty, is level once, not once a fetch
// interval.
func TestTheLoopSaysEachTimeItComesLevel(t *testing.T) {
	t.Parallel()
	var h *applyHarness
	told := make(chan uint64, 16)
	// stopOnTell, when armed, ends the run from inside the hook, so the loop
	// leaves between two fetches still level: the stop the restart below
	// must see past. READ BEFORE THE HOOK TELLS: the case arms it as soon as
	// the tell before the outage lands, so read after, that earlier call
	// could see it armed and end the run before the outage.
	var stopOnTell atomic.Pointer[context.CancelFunc]
	h = newApplyHarnessWith(t, probeDomain{}, func(d *statelog.RunnerDeps) {
		d.OnLevel = func() {
			stop := stopOnTell.Load()
			told <- h.runner.Committed().Seq
			if stop != nil {
				(*stop)()
			}
		}
	})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()
	awaitLevel(t, told, 1, "the first drain")
	// AN IDLE LOOP IS LEVEL ONCE: three more empty fetches, answered and
	// handled, tell nothing new.
	h.fetch.awaitAnswered(t, 3, "three idle fetches after the first drain")
	select {
	case seq := <-told:
		t.Fatalf("an idle loop said it came level again, at checkpoint %d", seq)
	default:
	}

	// A BURST OF WRITES, worked through: level again once it is applied.
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	awaitLevel(t, told, 2, "the end of a burst of writes")

	// A BROKER THAT STOPPED ANSWERING, retried across: level again on the
	// first answer after it, with nothing new to apply. The run ends there,
	// as the hook returns, before another fetch could clear the latch.
	stopOnTell.Store(&cancel)
	h.fetch.mu.Lock()
	h.fetch.failures = 2
	h.fetch.mu.Unlock()
	awaitLevel(t, told, 2, "the first answer after an outage")
	<-errs
	stopOnTell.Store(nil)

	// A RUN THAT STARTS AGAIN is level about a history of its own, however
	// the last one ended.
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	go func() { errs <- h.runner.Run(ctx) }()
	awaitLevel(t, told, 2, "the restarted loop's first drain")
	cancel()
	<-errs
}

// awaitLevel waits for one call of the level hook and holds it to the checkpoint
// at seq: told before its records commit, a woken gate would not find them.
func awaitLevel(t *testing.T, told <-chan uint64, seq uint64, what string) {
	t.Helper()
	select {
	case at := <-told:
		if at != seq {
			t.Fatalf("%s was told at checkpoint %d, want %d", what, at, seq)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("waited 15s for %s to be told", what)
	}
}

// awaitTold waits for one call of the drain hook and holds it to having been
// made with the latch already set.
func awaitTold(t *testing.T, told <-chan bool, what string) {
	t.Helper()
	select {
	case latched := <-told:
		if !latched {
			t.Fatalf("%s was told while Drained() still read false, so a "+
				"gate it woke would decline the copy it was woken for", what)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("waited 15s for %s to be told", what)
	}
}

// waitForDrain blocks until the runner's drain latch reads want, or fails.
func waitForDrain(t *testing.T, h *applyHarness, want bool, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if h.runner.Drained() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waited 15s for %s: Drained() = %v", what, h.runner.Drained())
}

// THE SWEEP RECORDS HOW FAR BACK IT FORGOT, and only when it forgot anything.
//
// The ledger's silence means "never applied here" only where the sweep has not
// deleted the row that would say otherwise, so the publisher reads this record
// before trusting it. A watermark
// trailing the rows deleted re-decides an operation that landed; one written by
// a pass that deleted nothing refuses operations whose rows are all here.
func TestTheOperationSweepRecordsWhatItForgot(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	rows, err := statelog.NewRows(h.estate.Reader(), probeDomain{}, specOf(probeDomain{}), nil)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	swept := func() (time.Time, bool) {
		t.Helper()
		before, ok, err := rows.LostBefore(t.Context(), "")
		if err != nil {
			t.Fatalf("read the sweep record: %v", err)
		}
		return before, ok
	}
	seed := func(appliedAt time.Time) {
		t.Helper()
		if err := h.estate.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `
				INSERT INTO probe_ops (op_id, subject, position, applied_at)
				VALUES (?, 'probe.o1', 1, ?)`,
				fmt.Sprintf("op-%d", appliedAt.UnixNano()),
				store.EncodeTime(appliedAt))
			return err
		}); err != nil {
			t.Fatalf("seed the ledger: %v", err)
		}
	}
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	// A PASS THAT DELETED NOTHING FORGOT NOTHING.
	seed(day)
	if _, err := h.runner.PurgeOps(t.Context(), day); err != nil {
		t.Fatalf("PurgeOps: %v", err)
	}
	if before, ok := swept(); ok {
		t.Fatalf("a sweep that deleted nothing recorded %s — every operation "+
			"minted before it is then refused although its row is here", before)
	}

	// A PASS THAT DELETED A ROW records its cutoff.
	cutoff := day.Add(time.Hour)
	if n, err := h.runner.PurgeOps(t.Context(), cutoff); err != nil || n != 1 {
		t.Fatalf("PurgeOps = (%d, %v), want the one row", n, err)
	}
	if before, ok := swept(); !ok || !before.Equal(cutoff) {
		t.Fatalf("the sweep record = (%s, %v), want %s — an operation minted "+
			"before it whose row it deleted is decided again", before, ok, cutoff)
	}

	// AND IT NEVER MOVES BACK: a later pass with an earlier cutoff — a
	// clock stepped back — does not un-forget what the first deleted.
	seed(day.Add(-time.Hour))
	if n, err := h.runner.PurgeOps(t.Context(), day); err != nil || n != 1 {
		t.Fatalf("PurgeOps = (%d, %v), want the one row", n, err)
	}
	if before, _ := swept(); !before.Equal(cutoff) {
		t.Fatalf("the sweep record moved back to %s from %s", before, cutoff)
	}
}

// A LOG'S BARRIERS ARE COUNTED WHERE THEY ARE APPLIED, ONCE EACH, UNDER THE LOG,
// AT THE HOUR THEY WERE COMMITTED.
//
// A barrier is what a linearizable read costs the log, and every data node
// applies every record, so the applier's count is the fleet's, which
// `census_drift` needs. Labelled with the stream an operator finds the log by;
// counted from the committed batch, so a redelivery below the checkpoint is no
// second read; windowed at the hour the BROKER stored it, so a replayed backlog
// is not read as the last day's reads. The cumulative series counts every
// barrier applied, for an exporter to diff.
func TestALogsBarriersAreCountedWhereTheyAreApplied(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	barrier := func(seq uint64) statelog.Envelope {
		return statelog.Envelope{
			V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
			Subject: statelog.Subject{Kind: statelog.BarrierKind}, Gen: 1,
			Scope:  statelog.ScopeSet{Paths: []string{statelog.FrameworkScope}},
			Writer: fmt.Sprintf("node-%d", seq),
		}
	}
	// SEQUENCES 1–4 ARE THREE DAYS OLD — a backlog this node replays on its
	// return — and 5–8 are the last minutes'. Barriers at 2, 4, 6 and 8.
	now := time.Now().UTC()
	stored := func(seq uint64) time.Time {
		if seq <= 4 {
			return now.Add(-72*time.Hour + time.Duration(seq)*time.Second)
		}
		return now.Add(-10*time.Minute + time.Duration(seq)*time.Second)
	}
	for seq := uint64(1); seq <= 8; seq++ {
		if seq%2 == 0 {
			h.fetch.offerStored(seq, stored(seq), barrier(seq))
			continue
		}
		h.fetch.offerStored(seq, stored(seq), env(seq, "edit", fmt.Sprintf("o%d", seq),
			fmt.Sprintf("op-%d", seq), 1))
	}
	if err := h.run(8); err != nil {
		t.Fatalf("run: %v", err)
	}
	// A REDELIVERY of a barrier the checkpoint already holds.
	h.fetch.offerStored(6, stored(6), barrier(6))
	h.fetch.offerStored(9, stored(9), env(9, "edit", "o9", "op-9", 1))
	if err := h.run(9); err != nil {
		t.Fatalf("run the redelivery: %v", err)
	}

	var applied uint64
	for _, snapshot := range h.metrics.Read() {
		if snapshot.Name == metrics.StatelogBarriersApplied {
			applied += snapshot.Total
		}
	}
	if applied != 4 {
		t.Errorf("the applier applied 4 barriers and the cumulative series "+
			"counts %d", applied)
	}
	var counted uint64
	for _, snapshot := range h.metrics.ReadWindow() {
		if snapshot.Name != metrics.StatelogBarriersApplied {
			continue
		}
		if got, want := snapshot.Attrs["stream"], specOf(probeDomain{}).Name; got != want {
			t.Errorf("barriers were counted under the stream %q, and the log is %q", got, want)
		}
		counted += snapshot.Total
	}
	if counted != 2 {
		t.Fatalf("the log took 2 barriers in the last day and the window counts "+
			"%d: a replayed backlog's barriers were filed as today's reads", counted)
	}
}
