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
