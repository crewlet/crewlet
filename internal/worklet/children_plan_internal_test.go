package worklet

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A TASK'S CHILDREN ARE READ IN THE TREE INDEX'S OWN ORDER.
//
// `tracker_tasks_parent_idx` carries the id after the parent (replicated 0058)
// because both readers that take one parent's children take them in id order:
// the apply's read of a removed task's children, and a cross-project move's
// walk, a batch at a time after the last id it carried. Over an index of the
// parent alone each seeks the children and then sorts them — and the walk sorts
// every child the parent has once per batch — which a check of the seek alone
// passes.
func TestATasksChildrenAreReadInTheTreesOrder(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	for name, read := range map[string]plannedRead{
		"a task's children": {childrenStatement, []any{"t-00001"}},
		"a walk's batch of children": {childBatchStatement,
			[]any{"t-00001", "P01", "", WalkBatch}},
	} {
		plan := storetest.Plan(t, db, read.sql, read.args)
		if !storetest.Seeks(plan, parentIndex) || storetest.Sorts(plan) {
			t.Errorf("%s is not a seek of %s in its own order:\n%s", name,
				parentIndex, strings.Join(plan, "\n"))
		}
	}
}
