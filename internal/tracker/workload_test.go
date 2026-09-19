package tracker_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func (r *roundTrip) workload(q tracker.WorkloadQuery) tracker.WorkloadAnswer {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	answer, err := r.reader.Workload(r.t.Context(), q, wednesday)
	if err != nil {
		r.t.Fatalf("Workload: %v", err)
	}
	return answer
}

// loadOf finds one person's row, or fails naming who is there instead.
func loadOf(t *testing.T, a tracker.WorkloadAnswer, handle string) tracker.WorkloadRow {
	t.Helper()
	for _, row := range a.Rows {
		if row.Handle == handle {
			return row
		}
	}
	held := []string{}
	for _, row := range a.Rows {
		held = append(held, row.Handle)
	}
	t.Fatalf("no workload row for %s; the answer names %v", handle, held)
	return tracker.WorkloadRow{}
}

// pointedTask files one sized, assigned task. The project defaults to ENG —
// the workload's own questions are about a person rather than a container, so
// naming one at every call site would be noise on every line but the two that
// are about the unit narrowing.
func pointedTask(t *testing.T, r *roundTrip, id string, points float64,
	assignee string, project ...string) {

	t.Helper()
	task := newTask(id)
	task.Key = "ENG-" + id
	task.Points = points
	task.Assignee = assignee
	if len(project) > 0 {
		task.Project = project[0]
		task.Key = project[0] + "-" + id
	}
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	r.drain()
}

// estimatedTask is its other half: sized in MINUTES rather than points, which
// is what a company using the other measure files.
func estimatedTask(t *testing.T, r *roundTrip, id string, minutes int,
	project, assignee string) {

	t.Helper()
	task := newTask(id)
	task.Project = project
	task.Key = project + "-" + id
	task.EstimateMinutes = minutes
	task.Assignee = assignee
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	r.drain()
}

// WHO IS CARRYING HOW MUCH, IN ONE READ.
//
// A caller asking it per project paid one round trip each and rewrote the
// arithmetic per surface. This is one read, over the same rows.
func TestTheWorkloadAnswersEverybodyAtOnce(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "ENG", Name: "ENG", Unit: "Core"})
	pointedTask(t, r, "a1", 8, "ada")
	pointedTask(t, r, "a2", 3, "ada")
	pointedTask(t, r, "b1", 2, "bo")
	// AND ONE THAT IS FINISHED. This answers what somebody is holding NOW,
	// not what they have ever held: counting a person's delivered work as
	// load would put every productive person at the top by the end of a
	// quarter, and nothing about it is load.
	pointedTask(t, r, "a3", 21, "ada")
	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-done", "a3", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done},
		tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("deliver a task: %v", err)
	}
	r.drain()

	answer := r.workload(tracker.WorkloadQuery{})
	ada := loadOf(t, answer, "ada")
	if ada.Open != 2 || ada.Points != 11 {
		t.Errorf("ada holds %d open worth %v points, want 2 and 11",
			ada.Open, ada.Points)
	}
	bo := loadOf(t, answer, "bo")
	if bo.Open != 1 || bo.Points != 2 {
		t.Errorf("bo holds %d open worth %v points, want 1 and 2",
			bo.Open, bo.Points)
	}
	// THE HEAVIEST FIRST, because the question is who is carrying the most.
	if got := answer.Rows[0].Handle; got != "ada" {
		t.Errorf("the answer leads with %q, want the person holding most", got)
	}
}

// BOTH MEASURES TRAVEL, never one chosen.
//
// Which of points and minutes a team sizes in is that team's own habit, so an
// answer that picked one would be wrong for everybody sizing in the other —
// and a person working across two teams sizing differently has BOTH numbers
// rather than a sum of them, because points are not minutes.
func TestTheWorkloadCarriesBothMeasures(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "ENG", Name: "ENG", Unit: "Core"})
	seedProject(t, r, tracker.Project{Key: "PROD", Name: "PROD", Unit: "Product"})
	pointedTask(t, r, "a1", 7, "ada")
	estimatedTask(t, r, "a2", 600, "PROD", "ada")

	ada := loadOf(t, r.workload(tracker.WorkloadQuery{}), "ada")
	if ada.Points != 7 {
		t.Errorf("ada holds %v points, want the 7 her pointed work is worth",
			ada.Points)
	}
	if ada.EstimateMinutes != 600 {
		t.Errorf("ada holds %d estimated minutes, want 600 — the other "+
			"measure is not a second field nobody reads", ada.EstimateMinutes)
	}
}

// THE SHAPES OF "NOT SIMPLY WORK IN PROGRESS" TRAVEL BESIDE THE TOTALS.
//
// A person whose whole queue is blocked has a different problem from one who
// is simply busy, and a total alone cannot tell them apart.
func TestTheLoadSaysWhatKindOfWorkItIs(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "ENG", Name: "ENG"})
	pointedTask(t, r, "dep", 1, "ada")
	pointedTask(t, r, "blk", 1, "bo")
	if _, err := r.writer.Depend(t.Context(), "op-depend", tracker.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()

	ada := loadOf(t, r.workload(tracker.WorkloadQuery{}), "ada")
	if ada.Blocked != 1 {
		t.Errorf("ada holds %d blocked of %d open, want the one that is",
			ada.Blocked, ada.Open)
	}
	if ada.Unscheduled != 1 {
		t.Errorf("ada holds %d unscheduled, want the one with neither date",
			ada.Unscheduled)
	}
	if ada.Overdue != 0 {
		t.Errorf("ada holds %d overdue with nothing dated at all", ada.Overdue)
	}
}

// AND A UNIT NARROWS TO THE WORK THAT UNIT OWNS.
func TestAUnitNarrowsTheLoad(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "ENG", Name: "ENG", Unit: "Core"})
	seedProject(t, r, tracker.Project{Key: "PROD", Name: "PROD", Unit: "Product"})
	pointedTask(t, r, "a1", 4, "ada")
	pointedTask(t, r, "a2", 9, "ada", "PROD")

	ada := loadOf(t, r.workload(tracker.WorkloadQuery{Unit: "Core"}), "ada")
	if ada.Points != 4 {
		t.Errorf("narrowed to Core, ada holds %v points — want only the work "+
			"the unit being shown owns", ada.Points)
	}
	// AND A UNIT NOBODY IS IN IS EMPTY rather than the whole company.
	if rows := r.workload(tracker.WorkloadQuery{Unit: "Legal"}).Rows; len(rows) != 0 {
		handles := []string{}
		for _, row := range rows {
			handles = append(handles, row.Handle)
		}
		slices.Sort(handles)
		t.Errorf("a unit with no work answers %v, want nobody", handles)
	}
}
