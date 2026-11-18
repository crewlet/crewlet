package worklet

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store/storetest"
)

// WHERE ONE TASK SITS IS ASKED OF THAT TASK, BY ITS KEY.
//
// The around read's probe of which run holds the task ([segmentHolds]) names
// the task, so it reads one row by the table's key whatever index the
// answer's listings use: through the trash's index ([taskSource]), which
// leads with the removal, it walked every removal in scope once per run. The
// subtree rollup's probe of the task's root seeks its key too, under an alias
// of its own ([anchorProbe]) that [tasksReadOtherwise] does not count.
func TestWhereATaskSitsIsAskedByItsKey(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	// A SEEK OF THE TABLE'S KEY ON THE ID, which `parent_id=?` would also
	// match as a substring.
	byKey := regexp.MustCompile(`^SEARCH t USING (INDEX sqlite_autoindex_tracker_tasks_1|PRIMARY KEY) \(id=\?\)`)
	for name, c := range map[string]struct {
		params map[string]any
		index  string
	}{
		"a project's trash": {map[string]any{"container": "project:P01"},
			projectRemovedIndex},
		"a project's trash, flat": {map[string]any{"container": "project:P01",
			"subtasks": "separate"}, projectRemovedIndex},
		"the company's trash": {map[string]any{}, removedIndex},
		"the company's trash, flat": {map[string]any{"subtasks": "separate"},
			removedIndex},
	} {
		t.Run(name, func(t *testing.T) {
			params := map[string]any{"removed": "true", "show_closed": "true"}
			for key, value := range c.params {
				params[key] = value
			}
			q, err := ParseQuery(MapParams(params), planNow, time.UTC)
			if err != nil {
				t.Fatalf("ParseQuery: %v", err)
			}
			set, err := compile(q, planNow, planFields(q))
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			statement, args := segmentHoldsStatement(segment{}, set, "t-00001")
			plan := storetest.Plan(t, db, statement, args)
			shown := strings.Join(plan, "\n")
			others, _ := tasksReadOtherwise(plan, c.index)
			keyed := slices.IndexFunc(others, byKey.MatchString)
			if keyed < 0 {
				t.Fatalf("where one removed task sits is not read by the task's "+
					"key:\n%s", shown)
			}
			if others = slices.Delete(others, keyed, keyed+1); len(others) > 0 {
				t.Errorf("where one removed task sits reads the task table other "+
					"than by the task's key and through %s:\n%s", c.index, shown)
			}
		})
	}
}

// A TASK'S NEIGHBOURS ARE SOUGHT FROM THE TASK, IN THE ORDER THE RUN IS WALKED.
//
// A filter by "after the task" or its negation cannot be sought, so each step
// through a board ([readAround]) would read the run again, and a heap-scan
// check misses it. So for every registered query, when the run's first row
// walks an index unsorted, every arm of the next neighbour and of the count
// ahead must SEEK it on the order's leading column, the neighbour unsorted;
// and when its last row walks one backwards unsorted, so must every arm of
// the previous neighbour. An order no index serves is exempt.
//
// NOT VACUOUS: the indexes the checked first and last rows walk must include
// every one named below, in each direction.
func TestAnAroundTasksNeighboursSeekTheOrder(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	walked := map[string]map[string]bool{runFirst: {}, runLast: {}}
	for name, params := range registeredQueries() {
		q := planQuery(t, params)
		statements := queryStatements(t, q)
		terms := sortTerms(q, planFields(q), planBasis())
		leading := strings.TrimPrefix(terms[0].Column, rowAlias.col(""))
		bound := regexp.MustCompile(`\(.*\b` + regexp.QuoteMeta(leading) + `[=<>]`)
		for _, side := range []struct {
			twin string
			arms []string
		}{
			{runFirst, []string{nextRow, rowsAhead}},
			{runLast, []string{previousRow}},
		} {
			twin := storetest.PlanTree(t, db, statements[side.twin].sql,
				statements[side.twin].args)
			top, read := topTaskRead(twin)
			if !read || sortsAtTop(twin) {
				continue
			}
			indexes := storetest.IndexesIn([]string{top.Detail})
			if len(indexes) != 1 {
				continue
			}
			index := indexes[0]
			walked[side.twin][index] = true
			t.Run(name+", beside "+side.twin, func(t *testing.T) {
				for _, arms := range side.arms {
					for i := 0; ; i++ {
						arm, ok := statements[aroundArm(arms, i)]
						if !ok {
							if i == 0 {
								t.Fatalf("no %s to plan", aroundArm(arms, i))
							}
							break
						}
						what := aroundArm(arms, i)
						tree := storetest.PlanTree(t, db, arm.sql, arm.args)
						shown := strings.Join(storetest.Details(tree), "\n")
						line, read := taskReadOf(tree)
						switch {
						case !read:
							t.Errorf("%s reads no task at all:\n%s", what, shown)
						case !strings.HasPrefix(line.Detail, "SEARCH ") ||
							!slices.Contains(storetest.IndexesIn([]string{line.Detail}), index) ||
							!bound.MatchString(line.Detail):
							t.Errorf("%s does not SEEK %s on %s, the index and "+
								"the column %s walks — so it walks the run to "+
								"the task:\n%s", what, index, leading, side.twin,
								shown)
						case arms != rowsAhead && sortsAtTop(tree):
							t.Errorf("%s sorts the run it read to find one "+
								"row:\n%s", what, shown)
						}
					}
				}
			})
		}
	}
	// SPELLED rather than taken from the constants, which name what is under
	// test.
	for _, twin := range []string{runFirst, runLast} {
		for _, index := range []string{"tracker_tasks_board_idx",
			"tracker_tasks_updated_idx", "tracker_tasks_recent_idx",
			"tracker_tasks_spend_idx", "tracker_tasks_removed_idx",
			"tracker_tasks_project_removed_idx"} {
			if !walked[twin][index] {
				t.Errorf("no registered query's %s walks %s unsorted, so this "+
					"test never checks a neighbour read in that order", twin,
					index)
			}
		}
	}
}

// taskReadOf is the statement's read of its rows wherever it sits in the
// plan: the statement's own, or — for a count, which reads its rows inside a
// subquery it then counts — the first line that reads the task table under
// the row's alias.
func taskReadOf(tree []storetest.PlanLine) (storetest.PlanLine, bool) {
	if line, ok := topTaskRead(tree); ok {
		return line, true
	}
	for _, line := range tree {
		if strings.HasPrefix(line.Detail, "SCAN tracker_tasks AS "+string(rowAlias)+" ") ||
			strings.HasPrefix(line.Detail, "SEARCH "+string(rowAlias)+" ") {
			return line, true
		}
	}
	return storetest.PlanLine{}, false
}
