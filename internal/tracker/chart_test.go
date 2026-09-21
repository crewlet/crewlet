package tracker_test

import (
	"database/sql"
	"testing"

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
	wrote, err := r.writer.ApplyChart(t.Context(), 100, chart)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(wrote) != 0 {
		t.Errorf("re-applying one revision wrote %v — every boot of every node "+
			"would put a record on the log for a value nobody changed", wrote)
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
