package tracker_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
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

	wrote, err := r.writer.ApplyChart(t.Context(), activatedAt(100), []tracker.ChartProject{
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

	if _, err := r.writer.ApplyChart(t.Context(), activatedAt(100), chart); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	r.drain()
	end := r.logEnd(t)
	wrote, err := r.writer.ApplyChart(t.Context(), activatedAt(100), chart)
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
	if wrote, err = r.writer.ApplyChart(t.Context(), activatedAt(101), changed); err != nil {
		t.Fatalf("third apply: %v", err)
	}
	if len(wrote) != 1 {
		t.Errorf("a renamed project was not written: %v", wrote)
	}
	r.drain()

	// AND AN OLDER REVISION ARRIVING LATE DOES NOT WALK IT BACK. Two
	// nodes applying two revisions is ordinary during a rollout, and the
	// node that is behind must not undo the one that is ahead.
	if wrote, err = r.writer.ApplyChart(t.Context(), activatedAt(100), chart); err != nil {
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

// A REAPPLY OF ONE ACTIVATION SETS RIGHT WHAT AN EQUAL ONE WALKED BACK —
// which is why a chart apply decides from the project's rows rather than
// letting the operation ledger answer it.
//
// The guard lets an EQUAL stamp through, so a stale write that lands at the
// same activation as the current one stands until the next apply puts it back.
// An operation id derived from the activation is one the ledger already holds
// from that activation's first write, so it answered the reapply as done and
// the stale names stood until the configuration moved again.
func TestAReapplySetsRightWhatAnEqualPositionWalkedBack(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	const at = int64(100)
	current := []tracker.ChartProject{{Key: "ENG", Name: "Engineering & Ops", Unit: "Eng"}}
	stale := []tracker.ChartProject{{Key: "ENG", Name: "Engineering", Unit: "Eng"}}

	if _, err := r.writer.ApplyChart(t.Context(), activatedAt(at), current); err != nil {
		t.Fatalf("the current chart's apply: %v", err)
	}
	r.drain()
	if _, err := r.writer.ApplyChart(t.Context(), activatedAt(at), stale); err != nil {
		t.Fatalf("the equal-position stale apply: %v", err)
	}
	r.drain()
	if name := r.projectName("ENG"); name != "Engineering" {
		t.Fatalf("the stale apply left %q — the premise of this case is an equal "+
			"position the guard lets through", name)
	}

	wrote, err := r.writer.ApplyChart(t.Context(), activatedAt(at), current)
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
	wrote, err := r.writer.ApplyChart(t.Context(), activatedAt(100), []tracker.ChartProject{
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

	wroteA, err := a.writer.ApplyChart(t.Context(), activatedAt(100), chart)
	if err != nil || len(wroteA) != 1 {
		t.Fatalf("node a's apply = (%v, %v), want [ENG]", wroteA, err)
	}
	end := a.logEnd(t)

	// NODE B HAS NOT APPLIED NODE A'S RECORD: it decides a create on rows
	// with no project in them.
	wroteB, err := b.writer.ApplyChart(t.Context(), activatedAt(100), chart)
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
	wrote, err := r.writer.ApplyChart(t.Context(), activatedAt(100), []tracker.ChartProject{
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

// A LATER ACTIVATION OVER UNCHANGED FIELDS RE-STAMPS THE ROW, so an older one
// applied late cannot walk it back.
//
// The stamp is compared BEFORE the fields. A row left at the older activation
// because its three fields happened to match the newer one is open to every
// activation between the two: one nobody applied before it was superseded,
// arriving late from a slow node, finds a row stamped below it and writes its
// own older names over the newer ones. Re-stamping costs one record per
// project per activation; reapplying ONE activation still costs nothing, which
// is what [TestReapplyingOneChartWritesNothing] holds.
func TestALaterActivationReStampsSoAnOlderOneCannotWalkItBack(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	chart := []tracker.ChartProject{{Key: "ENG", Name: "Engineering", Unit: "Eng"}}

	if _, err := r.writer.ApplyChart(t.Context(), activatedAt(100), chart); err != nil {
		t.Fatalf("the first activation: %v", err)
	}
	r.drain()
	wrote, err := r.writer.ApplyChart(t.Context(), activatedAt(300), chart)
	if err != nil {
		t.Fatalf("a later activation over the same fields: %v", err)
	}
	r.drain()
	if len(wrote) != 1 || r.projectChartEpoch("ENG") != 300 {
		t.Fatalf("a later activation over unchanged fields wrote %v and left the "+
			"row stamped %d, want it re-stamped at 300", wrote,
			r.projectChartEpoch("ENG"))
	}

	late := []tracker.ChartProject{{Key: "ENG", Name: "Engineering (old)", Unit: "Eng"}}
	if wrote, err = r.writer.ApplyChart(t.Context(), activatedAt(200), late); err != nil {
		t.Fatalf("the late activation: %v", err)
	}
	r.drain()
	if len(wrote) != 0 || r.projectName("ENG") != "Engineering" {
		t.Errorf("an activation older than the row's applied late wrote %v and "+
			"left the project named %q — it walked a newer configuration back",
			wrote, r.projectName("ENG"))
	}
}

// AND THE STAMP ARBITRATES A REAL DISAGREEMENT.
//
// Two nodes applying two revisions is ordinary during a rollout, and the node
// still on the older one must not walk back what the newer one wrote.
func TestABehindNodesReconcileDoesNotWalkBackAnAheadOnes(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)

	ahead := []tracker.ChartProject{{Key: "ENG", Name: "Engineering & Ops", Unit: "Eng"}}
	if _, err := r.writer.ApplyChart(t.Context(), activatedAt(900), ahead); err != nil {
		t.Fatalf("the ahead node's reconcile: %v", err)
	}
	r.drain()

	behind := []tracker.ChartProject{{Key: "ENG", Name: "Engineering", Unit: "Eng"}}
	wrote, err := r.writer.ApplyChart(t.Context(), activatedAt(100), behind)
	if err != nil {
		t.Fatalf("the behind node's reconcile: %v", err)
	}
	if len(wrote) != 0 {
		t.Errorf("a node on activation 100 overwrote what activation 900 wrote: %v", wrote)
	}
	r.drain()
	if name := r.projectName("ENG"); name != "Engineering & Ops" {
		t.Errorf("the project's name is %q — the node that was behind won", name)
	}
}

// AN APPLY WITH NO ACTIVATION IS REFUSED: there is no honest default, and a
// caller holding no activation has no chart to apply.
func TestAChartApplyWithNoActivationIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	_, err := r.writer.ApplyChart(t.Context(), time.Time{},
		[]tracker.ChartProject{{Key: "ENG", Name: "Engineering", Unit: "Eng"}})
	if !errors.Is(err, tracker.ErrNoChartActivation) {
		t.Fatalf("an apply with no activation = %v, want ErrNoChartActivation", err)
	}
}

// THE STAMP IS ON THE ROW, and the column the log-position guard used is not
// there at all.
//
// The guard is only worth having if a reader can see it: the reconcile compares
// the stored number against the activation it is applying, so a column the
// applier never filled would make every node's comparison read zero and every
// reconcile a write. And the column it replaced — a log position nothing
// writes any more — is gone, because a guard column nothing fills reads as a
// fact about the row and is exactly the kind of value a later reader compares.
func TestTheChartEpochIsWrittenOntoTheProjectRow(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)

	at := time.Date(2026, 3, 2, 10, 0, 0, 123_000_000, time.UTC)
	if _, err := r.writer.ApplyChart(t.Context(), at, []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering", Unit: "Eng"},
	}); err != nil {
		t.Fatalf("ApplyChart: %v", err)
	}
	r.drain()

	if got, want := r.projectChartEpoch("ENG"), configplane.ActivationStamp(at); got != want {
		t.Errorf("tracker_projects.chart_epoch = %d, want %d — the guard "+
			"the next reconcile compares is not on the row", got, want)
	}
	var count int
	if err := r.db.Replicated().Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(),
			`SELECT count(*) FROM pragma_table_info('tracker_projects')
			 WHERE name = 'chart_position'`).Scan(&count)
	}); err != nil {
		t.Fatalf("read the project table's columns: %v", err)
	}
	if count != 0 {
		t.Error("tracker_projects still carries chart_position, which nothing " +
			"writes: a guard column with no writer is a value every reader " +
			"is entitled to misread")
	}
}

// projectChartEpoch reads one project's chart guard straight out of the row.
func (r *roundTrip) projectChartEpoch(key string) int64 {
	r.t.Helper()
	var at int64
	if err := r.db.Replicated().Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(),
			`SELECT chart_epoch FROM tracker_projects WHERE key = ?`, key).Scan(&at)
	}); err != nil {
		r.t.Fatalf("read project %s: %v", key, err)
	}
	return at
}

// activatedAt is the activation instant whose stamp is the given number of
// Unix milliseconds — so a case reads in stamps, the unit the guard compares.
func activatedAt(stamp int64) time.Time { return time.UnixMilli(stamp).UTC() }
