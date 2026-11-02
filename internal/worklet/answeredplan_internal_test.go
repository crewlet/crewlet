package worklet

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE ANSWERED READ SEARCHES ITS INDEX, and the term that makes it so is one a
// reader would delete as redundant: `c.ask <> ”` beside `c.ask = ?`, because
// a planner matches a partial index by syntactic implication and a bound
// parameter could be the empty string. Without the index the read is a scan
// of every comment the company ever wrote. The follow-up read seeks the task's
// own history and the closing commit by its key, never the whole history.
func TestTheAnsweredReadSearchesItsIndex(t *testing.T) {
	t.Parallel()
	// AN EMPTY ESTATE, for [seededPlanStore]'s reason: no node runs
	// ANALYZE, so the plan over an empty file is the plan every node takes.
	db := seededPlanStore(t, func(context.Context, *sql.Tx, int) error { return nil })

	t.Run("the page", func(t *testing.T) {
		plan := storetest.Plan(t, db, answeredStatement,
			[]any{"jane", store.EncodeTime(planNow), MaxAnswered + 1})
		shown := strings.Join(plan, "\n")
		if !storetest.Seeks(plan, "tracker_comments_answered_asks_idx") ||
			storetest.ScansHeap(plan, "tracker_comments") {
			t.Errorf("the answered read does not seek "+
				"tracker_comments_answered_asks_idx:\n%s", shown)
		}
	})
	t.Run("the follow-up", func(t *testing.T) {
		plan := storetest.Plan(t, db, followUpStatement, []any{"t-1", "bo", "op-1"})
		shown := strings.Join(plan, "\n")
		if !storetest.Seeks(plan, "tracker_history_subject_idx") ||
			storetest.ScansHeap(plan, "tracker_history") {
			t.Errorf("the follow-up read does not seek the task's own history:\n%s",
				shown)
		}
	})
}
