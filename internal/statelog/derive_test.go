package statelog_test

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// derivingApplier is the probe applier with ONE derived column: probe_derived
// holds how many probe rows there were when the rules last ran over them. It
// is a value the history determines and no record carries — the shape of
// every real derived column.
type derivingApplier struct {
	*probeApplier
	version int

	mu        sync.Mutex
	rederived int
}

func (d *derivingApplier) DerivationVersion() int { return d.version }

func (d *derivingApplier) Rederive(ctx context.Context, tx *sql.Tx, _ statelog.ApplyOptions) (int, error) {
	d.mu.Lock()
	d.rederived++
	d.mu.Unlock()
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS probe_derived (id INTEGER PRIMARY KEY, rows INTEGER NOT NULL)`); err != nil {
		return 0, err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO probe_derived (id, rows) SELECT 1, COUNT(*) FROM probe_rows
		ON CONFLICT (id) DO UPDATE SET rows = excluded.rows`)
	return 1, err
}

func (d *derivingApplier) times() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rederived
}

// restartWith rebuilds the runner over the SAME database with the given
// applier — a node restarting on a build whose rules are those.
func (h *applyHarness) restartWith(applier statelog.Applier, probe *probeApplier) {
	h.t.Helper()
	h.applier, h.fetch = probe, newProbeFetch()
	recorder, err := metrics.New()
	if err != nil {
		h.t.Fatalf("recorder: %v", err)
	}
	h.metrics = recorder
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: probeDomain{}, Applier: applier, Fetch: h.fetch,
		DB: h.db.Replicated(), Generation: 1, Metrics: recorder,
	})
	if err != nil {
		h.t.Fatalf("NewRunner: %v", err)
	}
	h.runner = runner
}

// deriving restarts the harness on a build deriving at version.
func (h *applyHarness) deriving(version int) *derivingApplier {
	d := &derivingApplier{probeApplier: newProbeApplier(), version: version}
	h.restartWith(d, d.probeApplier)
	return d
}

func (h *applyHarness) storedDerivation() int {
	h.t.Helper()
	var v int
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT derivation FROM statelog_cursor WHERE stream = ?`, probeStream).Scan(&v)
	}); err != nil {
		h.t.Fatalf("read the derivation: %v", err)
	}
	return v
}

func (h *applyHarness) derivedRows() int {
	h.t.Helper()
	var v int
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT rows FROM probe_derived WHERE id = 1`).Scan(&v)
	}); err != nil {
		h.t.Fatalf("read the derived column: %v", err)
	}
	return v
}

// A DERIVED COLUMN IS RE-DERIVED ON THE FIRST APPLY AFTER AN UPGRADE — once,
// from the rows at the checkpoint, before any record is applied on top — and
// never again while the rules stay the same.
//
// Without it a build that adds a derived column maintains it incrementally
// over rows its predecessor wrote without it, and a build that changes a rule
// extends values the old rule wrote: either way every node's column is a mix
// of two rules in proportions set by when that node happened to upgrade, on
// tables the fleet asserts byte-identical.
func TestADerivedColumnIsRederivedOnFirstApplyAfterUpgrade(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	if err := h.run(2); err != nil {
		t.Fatalf("the old build's run: %v", err)
	}
	if got := h.storedDerivation(); got != 0 {
		t.Fatalf("a build that derives nothing stamped derivation %d, want 0", got)
	}

	// THE UPGRADE: a build deriving at 1 boots over the old build's rows.
	upgraded := h.deriving(1)
	h.fetch.offer(3, env(3, "edit", "c", "op-3", 1))
	if err := h.run(3); err != nil {
		t.Fatalf("the upgraded build's run: %v", err)
	}
	if got := upgraded.times(); got != 1 {
		t.Fatalf("the upgraded build re-derived %d time(s), want exactly 1", got)
	}
	if got := h.derivedRows(); got != 2 {
		t.Fatalf("the re-derivation saw %d row(s), want the 2 at the checkpoint — "+
			"it ran after a record was applied on top of the old rows", got)
	}
	if got := h.storedDerivation(); got != 1 {
		t.Fatalf("the checkpoint names derivation %d after the re-derivation, want 1", got)
	}

	// THE SAME RULES AGAIN: a restart re-derives nothing.
	same := h.deriving(1)
	h.fetch.offer(4, env(4, "edit", "d", "op-4", 1))
	if err := h.run(4); err != nil {
		t.Fatalf("the restart's run: %v", err)
	}
	if got := same.times(); got != 0 {
		t.Fatalf("a restart on unchanged rules re-derived %d time(s)", got)
	}

	// ROWS FROM A NEWER BUILD — an adopted artefact — are brought DOWN to
	// the rules this build maintains rather than extended with them.
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE statelog_cursor SET derivation = 5 WHERE stream = ?`, probeStream)
		return err
	}); err != nil {
		t.Fatalf("stage a newer build's rows: %v", err)
	}
	older := h.deriving(1)
	h.fetch.offer(5, env(5, "edit", "e", "op-5", 1))
	if err := h.run(5); err != nil {
		t.Fatalf("the older build's run: %v", err)
	}
	if got, stored := older.times(), h.storedDerivation(); got != 1 || stored != 1 {
		t.Fatalf("over rows derived at 5, a build deriving at 1 re-derived %d "+
			"time(s) and left derivation %d, want once and 1", got, stored)
	}
}

// A FRESH NODE'S FIRST CHECKPOINT NAMES THE RULES THAT WROTE ITS ROWS, so its
// next boot does not re-derive a history this build derived itself — and a
// Deriver that claims version 0, which is what "no known rules" is stored as,
// is refused at construction.
func TestAFreshCheckpointIsStampedWithTheRulesThatDerivedIt(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	first := h.deriving(3)
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	if err := h.run(1); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, stored := first.times(), h.storedDerivation(); got != 0 || stored != 3 {
		t.Fatalf("a fresh node re-derived %d time(s) and stamped %d, want 0 and 3",
			got, stored)
	}
	again := h.deriving(3)
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := again.times(); got != 0 {
		t.Fatalf("the fresh node's second boot re-derived %d time(s)", got)
	}

	_, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain:  probeDomain{},
		Applier: &derivingApplier{probeApplier: newProbeApplier()},
		Fetch:   newProbeFetch(), DB: h.db.Replicated(), Generation: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "derives at version 0") {
		t.Fatalf("a Deriver at version 0 was accepted: %v", err)
	}
}
