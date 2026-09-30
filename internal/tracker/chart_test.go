package tracker_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A PROJECT EXISTS BECAUSE THE CHART SAYS SO, and nothing else makes one.
//
// A create is a sequence: it takes the next number from its project's own
// counter and only then writes the task. So a project that is not an object
// refuses every write into it — which, before the chart apply was wired, is
// what a company that had just booted did with the first task anybody filed.
//
// Creating the project inside the create instead is the shape that gives two
// nodes two projects, two counters and two ENG-1s when they file at once, so
// the chart is the one writer.
func TestTheChartIsWhatMakesAProjectExist(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)

	// BEFORE: the project is not there and a create says so, as
	// unavailable rather than as a failure — this node cannot tell "never
	// created" from "not applied here yet", and only one of those is
	// worth retrying.
	_, err := r.writer.CreateTask(t.Context(), "op-early", newTask("t-early"), nil)
	if err == nil {
		t.Fatal("a task was filed into a project that does not exist")
	}

	wrote, err := r.writer.ApplyChart(t.Context(), 100, []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering", Purpose: "builds it", Unit: "Engineering"},
	})
	if err != nil {
		t.Fatalf("ApplyChart: %v", err)
	}
	if len(wrote) != 1 || wrote[0] != "ENG" {
		t.Fatalf("the chart apply wrote %v, want [ENG]", wrote)
	}
	r.drain()

	// AFTER: the same create lands, which is the whole point.
	got, err := r.writer.CreateTask(t.Context(), "op-late", newTask("t-late"), nil)
	if err != nil {
		t.Fatalf("CreateTask after the chart apply: %v", err)
	}
	if got.Key != "ENG-1" {
		t.Errorf("the first task in ENG was keyed %q", got.Key)
	}
}

// A SECOND APPLY OF ONE REVISION WRITES NOTHING.
//
// The apply runs on every config apply and on every boot, on every node. If
// each of those were a record, a fleet of five restarting would put five
// identical projects on the log and the trim would carry them for its whole
// window — for a value nobody changed.
func TestReapplyingOneChartWritesNothing(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	chart := []tracker.ChartProject{{Key: "ENG", Name: "Engineering", Unit: "Eng"}}

	if _, err := r.writer.ApplyChart(t.Context(), 100, chart); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	r.drain()
	end := r.logEnd(t)
	wrote, err := r.writer.ApplyChart(t.Context(), 100, chart)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(wrote) != 0 {
		t.Errorf("re-applying one revision wrote %v — every boot of every node "+
			"would put a record on the log for a value nobody changed", wrote)
	}
	if got := r.logEnd(t); got != end {
		t.Errorf("re-applying one revision put %d record(s) on the log", got-end)
	}

	// A LATER REVISION THAT CHANGES SOMETHING DOES write.
	changed := []tracker.ChartProject{{Key: "ENG", Name: "Engineering & Ops", Unit: "Eng"}}
	if wrote, err = r.writer.ApplyChart(t.Context(), 101, changed); err != nil {
		t.Fatalf("third apply: %v", err)
	}
	if len(wrote) != 1 {
		t.Errorf("a renamed project was not written: %v", wrote)
	}
	r.drain()

	// AND AN OLDER REVISION ARRIVING LATE DOES NOT WALK IT BACK. Two
	// nodes applying two revisions is ordinary during a rollout, and the
	// node that is behind must not undo the one that is ahead.
	if wrote, err = r.writer.ApplyChart(t.Context(), 100, chart); err != nil {
		t.Fatalf("stale apply: %v", err)
	}
	if len(wrote) != 0 {
		t.Errorf("an older chart overwrote a newer one: %v", wrote)
	}
	r.drain()

	if name := r.projectName("ENG"); name != "Engineering & Ops" {
		t.Errorf("the project's name is %q — the newer chart lost", name)
	}
}

// A REAPPLY AT ONE POSITION SETS RIGHT WHAT AN EQUAL POSITION WALKED BACK —
// which is why a chart apply decides from the project's rows rather than
// letting the operation ledger answer it.
//
// The guard lets an EQUAL position through, so a stale write that lands at the
// same number as the current one stands until the next apply puts it back. An
// operation id derived from the position is one the ledger already holds from
// that position's first write, so it answered the reapply as done and the
// stale names stood until the chart moved again.
func TestAReapplySetsRightWhatAnEqualPositionWalkedBack(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	const at = int64(100)
	current := []tracker.ChartProject{{Key: "ENG", Name: "Engineering & Ops", Unit: "Eng"}}
	stale := []tracker.ChartProject{{Key: "ENG", Name: "Engineering", Unit: "Eng"}}

	if _, err := r.writer.ApplyChart(t.Context(), at, current); err != nil {
		t.Fatalf("the current chart's apply: %v", err)
	}
	r.drain()
	if _, err := r.writer.ApplyChart(t.Context(), at, stale); err != nil {
		t.Fatalf("the equal-position stale apply: %v", err)
	}
	r.drain()
	if name := r.projectName("ENG"); name != "Engineering" {
		t.Fatalf("the stale apply left %q — the premise of this case is an equal "+
			"position the guard lets through", name)
	}

	wrote, err := r.writer.ApplyChart(t.Context(), at, current)
	if err != nil {
		t.Fatalf("the reapply: %v", err)
	}
	r.drain()
	if len(wrote) != 1 {
		t.Errorf("the reapply wrote %v, want [ENG]", wrote)
	}
	if name := r.projectName("ENG"); name != "Engineering & Ops" {
		t.Errorf("after reapplying the current chart the project is named %q — "+
			"the stale names stand until the chart moves again", name)
	}
}

// A CHART APPLY IS DECIDED WHATEVER THE OPERATION LEDGER HAS LOST — which is
// most companies' steady state: a chart nobody has changed for longer than the
// ledger keeps its rows.
//
// The operation is the APPLY, minted when it runs through the state log's own
// grammar. The id it replaced, `chart:<position>:<key>`, was outside that
// grammar, so the ledger read it as minted at the zero instant and could vouch
// for it on no node whose ledger had ever lost a row: every boot of every such
// node had every project answered `unknown` without being decided.
func TestAChartApplyIsDecidedWhateverTheLedgerLost(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	// THE LEDGER HAS LOST ROWS UP TO A MINUTE AGO — a sweep, say. A minute
	// rather than now, so the apply's own mint, which an id resolves to the
	// millisecond, is unambiguously after it.
	if err := statelog.RecordLedgerLoss(t.Context(), r.db.Replicated(),
		tracker.Domain{}, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("record the ledger's watermark: %v", err)
	}
	wrote, err := r.writer.ApplyChart(t.Context(), 100, []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering", Unit: "Eng"},
	})
	if err != nil {
		t.Fatalf("an apply on a node whose ledger lost rows: %v — it is a "+
			"reconcile decided from the project's rows, and nothing about it "+
			"needs the ledger to vouch", err)
	}
	if len(wrote) != 1 {
		t.Fatalf("the apply wrote %v, want [ENG]", wrote)
	}
	r.drain()
	if name := r.projectName("ENG"); name != "Engineering" {
		t.Errorf("the project is named %q", name)
	}
}

// TWO NODES APPLYING ONE CHART PUT ONE RECORD ON THE LOG — the second decided
// on rows that did not have the first's yet, lost the broker's arbitration,
// and re-decided on the rows the winner wrote. And it REPORTS that it wrote
// nothing: the round that lost had decided to write, and a flag that round set
// and the next never cleared told the caller it had.
func TestTwoNodesApplyingOneChartWriteOnce(t *testing.T) {
	t.Parallel()
	a := newRoundTripWithoutProject(t)
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node-b.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open node b's store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	b := newRoundTripOn(t, a.broker, a.log, db, "node-b")
	b.applyWhileWriting()
	chart := []tracker.ChartProject{{Key: "ENG", Name: "Engineering", Unit: "Eng"}}

	wroteA, err := a.writer.ApplyChart(t.Context(), 100, chart)
	if err != nil || len(wroteA) != 1 {
		t.Fatalf("node a's apply = (%v, %v), want [ENG]", wroteA, err)
	}
	end := a.logEnd(t)

	// NODE B HAS NOT APPLIED NODE A'S RECORD: it decides a create on rows
	// with no project in them.
	wroteB, err := b.writer.ApplyChart(t.Context(), 100, chart)
	if err != nil {
		t.Fatalf("node b's apply: %v — losing the arbitration to an identical "+
			"write is not a failure", err)
	}
	if len(wroteB) != 0 {
		t.Errorf("node b reports it wrote %v — its first decision lost the "+
			"arbitration and its second found the chart already there", wroteB)
	}
	if got := a.logEnd(t); got != end {
		t.Errorf("node b put %d record(s) on the log for a chart node a had "+
			"already written", got-end)
	}
	if name := b.projectName("ENG"); name != "Engineering" {
		t.Errorf("node b's project is named %q", name)
	}
}

// A CHART WRITE WHOSE OUTCOME IS UNKNOWN IS AN ERROR, never a project the
// caller logs as applied: the next apply decides it again, and only a caller
// told so can say that.
func TestAnUnknownChartWriteIsAnError(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	// THE LEDGER HAS LOST ROWS UP TO AN HOUR FROM NOW, so it can vouch for
	// no operation minted before then — which is every one this apply mints.
	if err := statelog.RecordLedgerLoss(t.Context(), r.db.Replicated(),
		tracker.Domain{}, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("record the ledger's watermark: %v", err)
	}
	wrote, err := r.writer.ApplyChart(t.Context(), 100, []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering", Unit: "Eng"},
	})
	if err == nil {
		t.Fatalf("an unknown outcome was reported as success (wrote %v)", wrote)
	}
}

// projectName reads one project's chart-owned name straight out of the rows,
// which is what a stale apply would have overwritten.
func (r *roundTrip) projectName(key string) string {
	r.t.Helper()
	var name string
	if err := r.db.Replicated().Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(),
			`SELECT name FROM tracker_projects WHERE key = ?`, key).Scan(&name)
	}); err != nil {
		r.t.Fatalf("read project %s: %v", key, err)
	}
	return name
}

// A RECONCILE AT A LATER POSITION WRITES NOTHING WHEN THE ROWS ALREADY MATCH.
//
// # The bug this pins
//
// The guard used to be "the row is stamped at exactly this number AND the
// three fields match", over a stamp that was the applying node's own wall
// clock. Every pass took a fresh reading, so the first clause was false on
// every pass after the first — and the reconcile rewrote every project the
// chart names, on every apply, on every boot, on every node, for ever. Two
// nodes seconds apart each held a stamp higher than the other's and rewrote
// the whole catalogue back and forth between them.
//
// It is worse now than it was then, which is why it is a case: the reconcile
// follows every published company, and a company is published by a chart write
// as well as by an activation — so the number moves when somebody edits a
// seat's job title in a unit that has no project at all.
//
// The fix is the order. The fields are compared FIRST, and a row that already
// says what the chart says has nothing for a position to arbitrate.
func TestAReconcileAtALaterPositionWritesNothingWhenTheRowsAlreadyMatch(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	chart := []tracker.ChartProject{{Key: "ENG", Name: "Engineering", Unit: "Eng"}}

	if _, err := r.writer.ApplyChart(t.Context(), 100, chart); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	r.drain()

	// EVERY LATER POSITION, which is what a company that is being used
	// looks like: each of these stands for a chart record that changed
	// something else entirely.
	for _, at := range []int64{101, 500, 1 << 40} {
		wrote, err := r.writer.ApplyChart(t.Context(), at, chart)
		if err != nil {
			t.Fatalf("reconcile at %d: %v", at, err)
		}
		if len(wrote) != 0 {
			t.Fatalf("a reconcile at position %d rewrote %v although the rows "+
				"already said it — every chart write in the company would put "+
				"one record per project on the log", at, wrote)
		}
		r.drain()
	}
}

// AND THE POSITION IS STILL WHAT ARBITRATES A REAL DISAGREEMENT.
//
// Two nodes derive the same chart from the same rows, so when their
// derivations disagree the one with the LOWER cursor is the one holding the
// older chart. That is the whole job of the number, and it is the half the
// case above must not have removed: comparing the fields first is only safe
// because a row that matches has nothing to walk back.
func TestABehindNodesReconcileDoesNotWalkBackAnAheadOnes(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)

	ahead := []tracker.ChartProject{{Key: "ENG", Name: "Engineering & Ops", Unit: "Eng"}}
	if _, err := r.writer.ApplyChart(t.Context(), 900, ahead); err != nil {
		t.Fatalf("the ahead node's reconcile: %v", err)
	}
	r.drain()

	behind := []tracker.ChartProject{{Key: "ENG", Name: "Engineering", Unit: "Eng"}}
	wrote, err := r.writer.ApplyChart(t.Context(), 100, behind)
	if err != nil {
		t.Fatalf("the behind node's reconcile: %v", err)
	}
	if len(wrote) != 0 {
		t.Errorf("a node at position 100 overwrote what position 900 wrote: %v", wrote)
	}
	r.drain()
	if name := r.projectName("ENG"); name != "Engineering & Ops" {
		t.Errorf("the project's name is %q — the node that was behind won", name)
	}
}

// THE POSITION IS ON THE ROW, and the column it replaced is not there at all.
//
// The guard is only worth having if a reader can see it: the reconcile compares
// the stored number against the chart it is applying, so a column the applier
// never filled would make every node's comparison read zero and every
// reconcile a write. And the column it replaced — a wall-clock reading with no
// writer since — is gone, because a guard column nothing fills reads as a fact
// about the row and is exactly the kind of value a later reader compares.
func TestTheChartPositionIsWrittenOntoTheProjectRow(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)

	const at = int64(1)<<40 | 7
	if _, err := r.writer.ApplyChart(t.Context(), at, []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering", Unit: "Eng"},
	}); err != nil {
		t.Fatalf("ApplyChart: %v", err)
	}
	r.drain()

	if got := r.projectChartPosition("ENG"); got != at {
		t.Errorf("tracker_projects.chart_position = %d, want %d — the guard "+
			"the next reconcile compares is not on the row", got, at)
	}
	var count int
	if err := r.db.Replicated().Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(),
			`SELECT count(*) FROM pragma_table_info('tracker_projects')
			 WHERE name = 'chart_epoch'`).Scan(&count)
	}); err != nil {
		t.Fatalf("read the project table's columns: %v", err)
	}
	if count != 0 {
		t.Error("tracker_projects still carries chart_epoch, which nothing " +
			"writes: a guard column with no writer is a value every reader " +
			"is entitled to misread")
	}
}

// projectChartPosition reads one project's chart guard straight out of the row.
func (r *roundTrip) projectChartPosition(key string) int64 {
	r.t.Helper()
	var at int64
	if err := r.db.Replicated().Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(),
			`SELECT chart_position FROM tracker_projects WHERE key = ?`, key).Scan(&at)
	}); err != nil {
		r.t.Fatalf("read project %s: %v", key, err)
	}
	return at
}
