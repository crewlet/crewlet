package worklet_test

import (
	"slices"
	"testing"
	"time"
)

// dated creates a task carrying a due date, or none.
func dated(t *testing.T, r *roundTrip, id, due string) {
	t.Helper()
	task := newTask(id)
	task.Key = "ENG-" + id
	if due != "" {
		at, err := time.Parse(time.DateOnly, due)
		if err != nil {
			t.Fatalf("parse %q: %v", due, err)
		}
		task.DueAt = &at
	}
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	r.drain()
}

// AN UNDATED TASK IS NOT THE SOONEST THING IN THE COMPANY.
//
// SQLite puts NULLs FIRST ascending; "soonest first" is about values, and a row
// with none goes last, in either direction.
func TestSortingByADateLeavesTheUndatedLast(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	dated(t, r, "late", "2031-05-20")
	dated(t, r, "none", "")
	dated(t, r, "soon", "2031-04-20")
	ascending := ids(r.ask(map[string]any{
		"container": "project:ENG", "sort": "due",
	}))
	if !slices.Equal(ascending, []string{"soon", "late", "none"}) {
		t.Errorf("sort=due answers %v, want the soonest first and the undated "+
			"last — an absent deadline is not an earlier one", ascending)
	}
	// AND LAST DESCENDING TOO.
	descending := ids(r.ask(map[string]any{
		"container": "project:ENG", "sort": "-due",
	}))
	if !slices.Equal(descending, []string{"late", "soon", "none"}) {
		t.Errorf("sort=-due answers %v, want the latest first and the undated "+
			"last", descending)
	}
}

// AND A PAGE BOUNDARY LANDS IN THE SAME PLACE THE ORDER PUT IT.
//
// The cursor's comparison is the order's own, written out again: where the two
// disagree, a paged answer silently skips what falls between them.
func TestPagingThroughADateSortLosesNobody(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	dated(t, r, "a", "2031-04-20")
	dated(t, r, "b", "2031-05-20")
	dated(t, r, "c", "")
	dated(t, r, "d", "")
	for _, sort := range []string{"due", "-due"} {
		seen := []string{}
		cursor := ""
		for range 6 {
			params := map[string]any{
				"container": "project:ENG", "sort": sort, "limit": "1",
			}
			if cursor != "" {
				params["cursor"] = cursor
			}
			answer := r.ask(params)
			seen = append(seen, ids(answer)...)
			if answer.NextCursor == "" {
				break
			}
			cursor = answer.NextCursor
		}
		slices.Sort(seen)
		if !slices.Equal(seen, []string{"a", "b", "c", "d"}) {
			t.Errorf("sort=%s paged one at a time yields %v, want every task "+
				"exactly once — a boundary inside the undated rows drops or "+
				"repeats them", sort, seen)
		}
	}
}
