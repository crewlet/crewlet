package usage_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// A PAGE OF FIRES ENDS AT ITS BYTES, SAYS MORE LIES PAST IT, AND THE SCROLL
// RESUMES AFTER ITS LAST FIRE: five fires under a bound two fit come back in
// pages of two, two and one, newest first, none twice.
//
// Mutations, each red: drop the Take check in ScheduleRuns (one page of five);
// leave a cut page's `more` false (the scroll stops after two).
func TestAPageOfFiresEndsAtItsBytes(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	base := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range 5 {
			at := base.Add(-time.Duration(i) * time.Minute)
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO usage_schedule_runs
					(day, node, scope_type, scope_id, name, fired_at, target,
					 outcome, trace_id, turn_id, version)
				VALUES (?, 'n1', 'role', 'pm', ?, ?, 'pm', 'fired', '', ?, 1)`,
				at.Format("2006-01-02"), "standup-"+strings.Repeat("s", 300), store.EncodeTime(at),
				fmt.Sprintf("turn-%d", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	whole, more, err := usage.ScheduleRuns(t.Context(), db.Replicated(), usage.ScheduleRunsQuery{Limit: 10})
	if err != nil || len(whole) != 5 || more {
		t.Fatalf("the unbounded page holds %d fires, more %v, %v; want all five", len(whole), more, err)
	}
	heaviest := 0
	for _, run := range whole {
		raw, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		heaviest = max(heaviest, len(raw))
	}
	ctx := statelog.WithAnswerBytes(t.Context(), 2*heaviest+heaviest/2)
	var seen []string
	var sizes []int
	q := usage.ScheduleRunsQuery{Limit: 10}
	for range 10 {
		runs, more, err := usage.ScheduleRuns(ctx, db.Replicated(), q)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			seen = append(seen, run.TurnID)
		}
		sizes = append(sizes, len(runs))
		if !more {
			break
		}
		last := runs[len(runs)-1]
		q.BeforeAt, q.BeforeKey = last.FiredAt, last.Key()
	}
	want := []string{"turn-0", "turn-1", "turn-2", "turn-3", "turn-4"}
	if !slices.Equal(seen, want) || !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Errorf("scrolled %v in pages of %v, want %v in pages of [2 2 1]", seen, sizes, want)
	}
}
