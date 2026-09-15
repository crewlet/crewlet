package tracker_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// running is the sprint number every seeded policy points at. The workload
// reads the POINTER rather than the sprint's own row — "is this project in a
// sprint" is what decides whether its capacity counts.
var running = 1

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

// sprinting seeds a project running a sprint, with the capacities it declares.
func sprinting(t *testing.T, r *roundTrip, key, unit string,
	measure tracker.SprintMeasure, capacity map[string]tracker.Capacity) {

	t.Helper()
	seedProject(t, r, tracker.Project{
		Key: key, Name: key, Unit: unit, ActiveSprint: &running,
		Sprints: &tracker.SprintPolicy{
			LengthDays: 14, Next: 2, Measure: measure, Capacity: capacity,
		}})
}

// WHO IS CARRYING HOW MUCH, IN ONE READ.
//
// The two halves of the question live apart: what somebody HOLDS is a group-by
// over the tasks, and what they CAN hold is a sprint policy — per project,
// which is what `work_sprints` answers one of. A caller that summed them itself
// paid one round trip per project and rewrote the three-valued capacity per
// surface.
func TestTheWorkloadAnswersEverybodyAtOnce(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	sprinting(t, r, "ENG", "Core", tracker.MeasurePoints,
		map[string]tracker.Capacity{"ada": {Points: 5}})
	pointedTask(t, r, "a1", 8, nil, "ada")
	pointedTask(t, r, "a2", 3, nil, "ada")
	pointedTask(t, r, "b1", 2, nil, "bo")
	// AND ONE THAT IS FINISHED. This answers what somebody is holding NOW,
	// not what they have ever held: a person's delivered work counted
	// against their capacity would put every productive person over by the
	// end of a quarter, and nothing about it is load.
	pointedTask(t, r, "a3", 21, nil, "ada")
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
	if ada.Capacity == nil || *ada.Capacity != 5 {
		t.Fatalf("ada's capacity is %v, want the 5 her project declared",
			ada.Capacity)
	}
	if !ada.OverCapacity {
		t.Error("ada holds 11 points of a capacity of 5 and is not over — " +
			"the comparison is what the whole answer is for")
	}
	// AND AN UNDECLARED CAPACITY IS AN ABSENCE, never a zero: a policy
	// naming nobody would otherwise put every person permanently over,
	// which is the one failure that makes the screen useless.
	bo := loadOf(t, answer, "bo")
	if bo.Capacity != nil || bo.OverCapacity {
		t.Errorf("bo carries capacity %v over=%v with none declared for him",
			bo.Capacity, bo.OverCapacity)
	}
	// THE HEAVIEST FIRST, because the question is who is overloaded.
	if got := answer.Rows[0].Handle; got != "ada" {
		t.Errorf("the answer leads with %q, want the person holding most", got)
	}
}

// A CAPACITY IS A SUM ACROSS THE PROJECTS SOMEBODY WORKS IN.
//
// A person working across two projects has a capacity in each, and their
// capacity is the total — that is what the number means. Picking one silently
// answers about part of somebody's week.
func TestACapacityIsSummedAcrossProjects(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	sprinting(t, r, "ENG", "Core", tracker.MeasurePoints,
		map[string]tracker.Capacity{"ada": {Points: 5}})
	sprinting(t, r, "PROD", "Product", tracker.MeasurePoints,
		map[string]tracker.Capacity{"ada": {Points: 3}})
	pointedTask(t, r, "a1", 7, nil, "ada")

	ada := loadOf(t, r.workload(tracker.WorkloadQuery{}), "ada")
	if ada.Capacity == nil || *ada.Capacity != 8 {
		t.Fatalf("ada's capacity is %v, want the 5 and the 3 added up",
			ada.Capacity)
	}
	if ada.CapacityFrom != 2 {
		t.Errorf("the capacity says it came from %d projects, want 2 — a "+
			"reader cannot otherwise tell a whole week from part of one",
			ada.CapacityFrom)
	}
	if ada.OverCapacity {
		t.Error("ada holds 7 points of a summed capacity of 8 and reads as " +
			"over — the sum is what she is held to")
	}
}

// A PROJECT BETWEEN SPRINTS DECLARES NOTHING THIS ANSWER CAN USE.
//
// A capacity is a statement about a fortnight. Counting a dormant project's
// policy holds somebody to a number nobody is currently working to.
func TestOnlyARunningSprintsCapacityCounts(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "ENG", Name: "ENG",
		Sprints: &tracker.SprintPolicy{
			LengthDays: 14, Next: 2, Measure: tracker.MeasurePoints,
			Capacity: map[string]tracker.Capacity{"ada": {Points: 5}},
		}})
	pointedTask(t, r, "a1", 9, nil, "ada")

	ada := loadOf(t, r.workload(tracker.WorkloadQuery{}), "ada")
	if ada.Capacity != nil {
		t.Errorf("ada is held to %v with no sprint running — a capacity is a "+
			"statement about a fortnight", *ada.Capacity)
	}
}

// TWO MEASURES DO NOT ADD UP, AND THAT IS ITS OWN FACT.
//
// Points are not minutes. A sum across them is a number that means nothing, and
// leaving the capacity absent without saying why is indistinguishable from
// nobody having declared one — different facts, and only one is somebody's to
// fix.
func TestMixedMeasuresRefuseToBeSummed(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	sprinting(t, r, "ENG", "Core", tracker.MeasurePoints,
		map[string]tracker.Capacity{"ada": {Points: 5}})
	sprinting(t, r, "PROD", "Product", tracker.MeasureEstimate,
		map[string]tracker.Capacity{"ada": {EstimateMin: 600}})
	pointedTask(t, r, "a1", 7, nil, "ada")

	ada := loadOf(t, r.workload(tracker.WorkloadQuery{}), "ada")
	if ada.Capacity != nil {
		t.Errorf("ada's two capacities were added into %v — points are not "+
			"minutes", *ada.Capacity)
	}
	if !ada.MixedMeasures {
		t.Error("the answer does not say WHY the capacity is absent — " +
			"\"nobody said\" and \"it cannot be added up\" send a reader to " +
			"two different places")
	}
}

// THE SHAPES OF "NOT SIMPLY WORK IN PROGRESS" TRAVEL BESIDE THE TOTAL.
//
// A person at capacity whose whole queue is blocked has a different problem
// from one who is simply busy, and a total alone cannot tell them apart.
func TestTheLoadSaysWhatKindOfWorkItIs(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "ENG", Name: "ENG"})
	pointedTask(t, r, "dep", 1, nil, "ada")
	pointedTask(t, r, "blk", 1, nil, "bo")
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

// AND A UNIT NARROWS BOTH HALVES TOGETHER.
//
// Narrowing the tasks without narrowing the capacities would hold somebody to
// a number covering work the answer does not show.
func TestAUnitNarrowsTheLoadAndTheCapacityAlike(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	sprinting(t, r, "ENG", "Core", tracker.MeasurePoints,
		map[string]tracker.Capacity{"ada": {Points: 5}})
	sprinting(t, r, "PROD", "Product", tracker.MeasurePoints,
		map[string]tracker.Capacity{"ada": {Points: 3}})
	pointedTask(t, r, "a1", 4, nil, "ada")

	ada := loadOf(t, r.workload(tracker.WorkloadQuery{Unit: "Core"}), "ada")
	if ada.Capacity == nil || *ada.Capacity != 5 {
		t.Fatalf("narrowed to Core, ada's capacity is %v — want only the "+
			"capacity of the unit whose work is being shown", ada.Capacity)
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
