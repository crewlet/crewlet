package worklet

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// planFlow is the flow every planned statement reads: a live posture with a
// fact in each live tier, so a plan is of the whole expression.
var planFlow = flowAt{now: planNow, posture: Posture{Live: true, Seats: map[string]SeatPosture{
	"ada": {On: []PostureItem{{Item: "t-00001", Cause: FlowCauseTurn, Since: planNow.Add(-time.Minute)}},
		Parked: "t-00002", ParkedSince: planNow.Add(-time.Hour)},
	"bo": {Stop: FlowCauseLimit, StopSince: planNow.Add(-time.Hour), StopUntil: planNow.Add(time.Hour)},
}}}

// detailFlowStatementFor is the detail's flow read of one task, with its
// arguments, for the plan tests' registered reads.
func detailFlowStatementFor(task string) (string, []any) {
	statement, args := detailFlowStatement(planFlow)
	return statement, append(args, task)
}

// flowIndexes is the index each table the flow reads is sought through: its
// facts are index seeks keyed by the item's row, never a walk of a table.
// Spelled rather than read off the statement, since they are what is under
// test.
var flowIndexes = map[string][]string{
	"tracker_task_deps":   {"tracker_task_deps_open_idx"},
	"tracker_comments":    {"tracker_comments_open_asks_idx"},
	"tracker_rosters":     {"sqlite_autoindex_tracker_rosters_1"},
	"tracker_seat_places": {"sqlite_autoindex_tracker_seat_places_1"},
	"tracker_turns":       {"tracker_turns_task_effective_idx"},
	"tracker_history":     {"tracker_history_subject_idx"},
	"tracker_projects":    {"sqlite_autoindex_tracker_projects_1"},
}

// THE FLOW IS READ IN THE PAGE'S OWN STATEMENT, and the detail's and
// Workload's: no reader adds a statement per row, and every fact the
// expression reads is a seek of an index keyed by the item's row, so a page
// costs its rows' seeks and Workload its in-progress items'. A fact read by
// a walk of its table, or answered from its whole table by the planner's
// unnesting of a correlated subquery ([storetest.Decorrelated]), fails here.
func TestTheFlowIsReadInThePagesOwnStatement(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	board := tasksWhere("t.removed_at IS NULL AND t.project_key = ? AND t.status_group = ?",
		[]any{"P01", string(GroupActive)})
	terms := []sortTerm{{Column: "t.rank"}, {Column: "t.id"}}
	page, pageArgs := taskRowsStatement(board, terms, planFlow)
	detail, detailArgs := detailFlowStatementFor("t-00001")
	census, censusArgs := workloadStatement(nil, store.EncodeTime(planNow), planFlow)
	for name, read := range map[string]plannedRead{
		"a board's page": {page, append(pageArgs, PageDefault+1)},
		"the detail":     {detail, detailArgs},
		"Workload":       {census, censusArgs},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(read.sql, "tracker_seat_places") {
				t.Fatalf("this statement does not read the flow at all:\n%s", read.sql)
			}
			plan := storetest.Plan(t, db, read.sql, read.args)
			for table, indexes := range flowIndexes {
				if storetest.ScansHeap(plan, table) {
					t.Errorf("the flow reads %s with no index:\n%s", table, strings.Join(plan, "\n"))
				}
				sought := false
				for _, index := range indexes {
					sought = sought || storetest.Seeks(plan, index)
				}
				if !sought {
					t.Errorf("the flow does not seek %s through %v:\n%s", table, indexes,
						strings.Join(plan, "\n"))
				}
			}
			if line, unnested := storetest.Decorrelated(plan); unnested {
				t.Errorf("the flow answers a per-row fact from a whole table (%q):\n%s",
					line, strings.Join(plan, "\n"))
			}
		})
	}
}
