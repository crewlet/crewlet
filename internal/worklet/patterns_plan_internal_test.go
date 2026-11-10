package worklet

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE PATTERNS READ SEEKS ONE KIND'S WINDOW of comments, and every row it joins
// by key: the advisor reads it on every page load, over a company's whole history.
func TestTheWorkPatternsReadSeeksItsWindow(t *testing.T) {
	t.Parallel()
	db := seededPlanStore(t, func(context.Context, *sql.Tx, int) error { return nil })
	window := []any{store.EncodeTime(planNow.AddDate(0, 0, -7)), store.EncodeTime(planNow)}
	for name, statement := range map[string]string{
		"the agent comments and their mentions": patternNamedStatement,
		"the answers":                           patternAnswersStatement,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan := storetest.Plan(t, db, statement, window)
			shown := strings.Join(plan, "\n")
			if !storetest.Seeks(plan, "tracker_comments_kind_created_idx") {
				t.Errorf("the read does not seek tracker_comments_kind_created_idx:\n%s", shown)
			}
			for _, table := range []string{"tracker_comments", "tracker_notifications", "tracker_tasks"} {
				if storetest.ScansHeap(plan, table) {
					t.Errorf("the read scans %s:\n%s", table, shown)
				}
			}
			if line, unnested := storetest.Decorrelated(plan); unnested {
				t.Errorf("the read answers a per-row check from its whole table (%q):\n%s", line, shown)
			}
		})
	}
}

// A PERSON'S NOTICE LOAD IS A RANGE OF THEIR WEEK, on an index that covers it:
// never their whole retained inbox, nor every notice the company was sent.
func TestTheNoticeLoadReadSeeksThePersonsInbox(t *testing.T) {
	t.Parallel()
	db := seededPlanStore(t, func(context.Context, *sql.Tx, int) error { return nil })
	plan := storetest.Plan(t, db, patternNoticesStatement,
		[]any{"owner", store.EncodeTime(planNow.AddDate(0, 0, -7)), store.EncodeTime(planNow)})
	covered := slices.ContainsFunc(plan, func(line string) bool {
		return strings.Contains(line, "COVERING INDEX tracker_notifications_load_idx (recipient=? AND created_at>=? AND created_at<?)")
	})
	if !covered {
		t.Errorf("the notice load read is no covering range of the person's week:\n%s", strings.Join(plan, "\n"))
	}
}
