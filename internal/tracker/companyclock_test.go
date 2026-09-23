package tracker_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE DUE BANDS, A PERSON'S DAY AND THE WORKLOAD ARE CUT AT THE COMPANY'S
// MIDNIGHT — ALL THREE, ON ONE CLOCK (ADR-0018).
//
// 23:30 on Wednesday 16 April in Los Angeles is 06:30 on Thursday in UTC. A
// task due at noon on that Wednesday is due TODAY on the company's calendar,
// and overdue on UTC's. The board's due band, the overdue mark on the same row
// in a person's own day, and that person's overdue count on the workload are
// three readers of one day, and every one of them was once cut on UTC — so for
// the last seven hours of every Los Angeles day a board said "today" beside a
// row marked overdue.
//
// The UTC half of each assertion is the control: it is what proves the case
// sits inside the window where the two clocks disagree, so a reader that
// ignored the zone it was handed cannot pass by coincidence.
func TestDueBandsCutAtCompanyMidnight(t *testing.T) {
	t.Parallel()
	losAngeles, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	// 23:30 on Wednesday 16 April 2031, Pacific daylight time.
	now := time.Date(2031, time.April, 16, 23, 30, 0, 0, losAngeles).UTC()
	noon := time.Date(2031, time.April, 16, 12, 0, 0, 0, losAngeles).UTC()

	h := newReadHarness(t)
	h.seed("due-today", func(task *tracker.Task) {
		task.DueAt = &noon
		task.Assignee = "ana"
	})

	// THE BOARD'S BAND, through the grammar every surface parses with.
	band := func(loc *time.Location) string {
		t.Helper()
		q, err := tracker.ParseQuery(tracker.MapParams{
			"container": "project:ENG", "group_by": "due:bucket",
		}, now, loc)
		if err != nil {
			t.Fatalf("ParseQuery: %v", err)
		}
		q.Level = statelog.ReadStale
		answer, err := h.reader.Tasks(t.Context(), q, now)
		if err != nil {
			t.Fatalf("Tasks: %v", err)
		}
		key, held := bandOf(t, answer, "due-today")
		if !held {
			t.Fatal("the task is in no band at all")
		}
		return key
	}
	// THE PERSON'S OWN DAY: the overdue mark on the row they are assigned.
	markedOverdue := func(loc *time.Location) bool {
		t.Helper()
		day, err := h.reader.MyWork(t.Context(), tracker.MyWorkQuery{
			Who: tracker.PartyOf("ana"), Level: statelog.ReadStale,
		}, now, loc)
		if err != nil {
			t.Fatalf("MyWork: %v", err)
		}
		for _, row := range day.Assigned {
			if row.ID == "due-today" {
				return row.Overdue
			}
		}
		t.Fatalf("the task is not in ana's assigned block: %+v", day.Assigned)
		return false
	}
	// THE WORKLOAD: that person's overdue count.
	overdueCount := func(loc *time.Location) int {
		t.Helper()
		load, err := h.reader.Workload(t.Context(), tracker.WorkloadQuery{
			Level: statelog.ReadStale,
		}, now, loc)
		if err != nil {
			t.Fatalf("Workload: %v", err)
		}
		return loadOf(t, load, "ana").Overdue
	}

	if got := band(losAngeles); got != "today" {
		t.Errorf("on the company's clock the board bands the task %q, want today", got)
	}
	if markedOverdue(losAngeles) {
		t.Error("on the company's clock ana's own day marks the task overdue, " +
			"beside a board that says it is due today")
	}
	if got := overdueCount(losAngeles); got != 0 {
		t.Errorf("on the company's clock ana's workload counts %d overdue, want 0", got)
	}

	// The control: on UTC it is already Thursday.
	if got := band(time.UTC); got != "overdue" {
		t.Fatalf("on UTC the board bands the task %q, want overdue — the case "+
			"is not inside the window where the two clocks disagree", got)
	}
	if !markedOverdue(time.UTC) {
		t.Fatal("on UTC ana's own day does not mark the task overdue, so this " +
			"case cannot tell a reader that ignores its clock from one that " +
			"reads it")
	}
	if got := overdueCount(time.UTC); got != 1 {
		t.Fatalf("on UTC ana's workload counts %d overdue, want 1", got)
	}
}
