package worklet

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// WHERE ONE TASK SITS IS ASKED OF THAT TASK, BY ITS KEY.
//
// The around read first asks which run of an answer holds the task
// ([segmentHolds]), and that probe names the task, so its read of the task
// table is one row by the table's own key whatever index the answer's
// listings are read through. Measured when the trash's set began naming its
// index ([taskSource]): the probe was read through it too, and the trash's
// index leads with the removal rather than the id, so asking where one
// removed task sat walked every removal in scope once per run of the answer.
//
// So the probe's own read of the task is a seek of the key, and so is the
// subtree rollup's probe of that task's root, which reads the root under an
// alias of its own ([anchorProbe]) and which [tasksReadOtherwise], reading only
// the rows' own alias, does not count.
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
			plan := explain(t, db, statement, args)
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
