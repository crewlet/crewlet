package tracker_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A SPRINT'S ARITHMETIC IS ANCHORED ON THE BROKER'S OWN CLOCK, not on the
// harness's fixed `wednesday`.
//
// Every instant a sprint figure is a predicate over — a stay's `from_at`, a
// span's `entered_at` — is the record's EFFECTIVE instant, which the applier
// takes from the broker rather than from any authored field (D141, and the
// reason a writer with a skewed clock cannot put a completion in the wrong
// sprint). So a case that wants a stay INSIDE a window has to place the window
// around the instant its own records will actually carry, which is now.
func (r *roundTrip) sprints(q tracker.SprintQuery, now time.Time) tracker.SprintListing {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	listing, err := r.reader.Sprints(r.t.Context(), q, now)
	if err != nil {
		r.t.Fatalf("Sprints(%+v): %v", q, err)
	}
	return listing
}

// seedSprintWindow files one sprint with an explicit window and state.
func seedSprintWindow(t *testing.T, r *roundTrip, number int,
	state tracker.SprintState, start, end time.Time, closed *time.Time) {

	t.Helper()
	sprint := tracker.Sprint{
		V: 1, Project: "ENG", Number: number,
		Name: "Sprint " + itoa(number), State: state,
		StartAt: start, EndAt: end, ClosedAt: closed,
		CreatedAt: start, UpdatedAt: start,
	}
	if closed != nil {
		sprint.ClosedBy = "system"
	}
	if _, err := r.writer.WriteDocument(t.Context(), "op-sw-"+itoa(number),
		tracker.SprintSubject("ENG", number), "", sprint, tracker.ChangeSprintMinted, nil); err != nil {
		t.Fatalf("seed sprint %d: %v", number, err)
	}
	r.drain()
}

// pointedTask files a task with points into a sprint.
func pointedTask(t *testing.T, r *roundTrip, id string, points float64,
	sprint *int, assignee string) {

	t.Helper()
	task := newTask(id)
	task.Key = "ENG-" + id
	task.Points = points
	task.Sprint = sprint
	task.Assignee = assignee
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	r.drain()
}

// EVERY SPRINT FIGURE IS DERIVED FROM THE STAYS, and none of them is stored.
//
// Committed is a statement about what the sprint held at its START; a counter
// maintained forward is a statement about what it holds NOW, and the two
// differ for every task that arrived late — which is precisely the number a
// team wants.
func TestASprintReportSeparatesCommittedFromAdded(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	// A WINDOW THIS TEST'S OWN RECORDS FALL INSIDE — see [roundTrip.sprints].
	seedSprintWindow(t, r, 1, tracker.SprintActive, base.Add(-time.Hour),
		base.Add(13*24*time.Hour), nil)
	one := 1

	pointedTask(t, r, "a", 3, &one, "ada")
	pointedTask(t, r, "b", 5, &one, "bo")

	listing := r.sprints(tracker.SprintQuery{Project: "ENG"}, base)
	if len(listing.Sprints) != 1 {
		t.Fatalf("the report covers %d sprints, want 1", len(listing.Sprints))
	}
	got := listing.Sprints[0]
	// EVERY STAY OPENED AFTER THE START, so all eight points are ADDED and
	// none is committed. A reader that counted membership rather than the
	// stay's own instant would report all eight as committed.
	if got.Figures.Added != 8 {
		t.Fatalf("added is %v, want 8 — every stay opened inside the window",
			got.Figures.Added)
	}
	if got.Figures.Committed != 0 {
		t.Fatalf("committed is %v, want 0 — nothing was in the sprint when it "+
			"started, and committed is a statement about the START",
			got.Figures.Committed)
	}
	if got.Figures.Tasks != 2 {
		t.Fatalf("the sprint holds %d tasks, want 2", got.Figures.Tasks)
	}
	if got.Figures.Measure != tracker.MeasurePoints {
		t.Fatalf("the measure is %q, want points — a bare number is points to "+
			"one team and minutes to another", got.Figures.Measure)
	}
	// AND NOTHING IS DELIVERED YET, so remaining is the whole of it.
	if got.Figures.Done != 0 || got.Figures.Remaining != 8 {
		t.Fatalf("done=%v remaining=%v, want 0 and 8",
			got.Figures.Done, got.Figures.Remaining)
	}
}

// DONE IS A SPAN, NOT A STAY — delivery is not membership.
//
// A task can sit in a sprint all the way through and never be finished, and it
// can be finished in a sprint it joined an hour before the close. A reader
// that summed the delivered MEMBERS would also count a task delivered in a
// previous sprint and carried over.
func TestASprintsDeliveryIsCountedFromItsSpans(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	seedSprintWindow(t, r, 1, tracker.SprintActive, base.Add(-time.Hour),
		base.Add(13*24*time.Hour), nil)
	one := 1
	pointedTask(t, r, "ship", 5, &one, "ada")
	pointedTask(t, r, "stuck", 3, &one, "ada")

	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-ship", "ship", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("finish a task: %v", err)
	}
	r.drain()

	got := r.sprints(tracker.SprintQuery{Project: "ENG"}, base).Sprints[0]
	if got.Figures.Done != 5 {
		t.Fatalf("done is %v, want the 5 points of the task that shipped",
			got.Figures.Done)
	}
	if got.Figures.Remaining != 3 {
		t.Fatalf("remaining is %v, want the 3 points still open",
			got.Figures.Remaining)
	}

	// CANCELLED IS NOT DELIVERED. It is a FINISHED group, so a reader
	// counting finished groups would score a team that cancelled its
	// remaining work at a hundred per cent.
	cancelled := tracker.StatusCancelled
	if _, err := r.writer.UpdateTask(t.Context(), "op-drop", "stuck", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &cancelled}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("cancel a task: %v", err)
	}
	r.drain()
	after := r.sprints(tracker.SprintQuery{Project: "ENG"}, base).Sprints[0]
	if after.Figures.Done != 5 {
		t.Fatalf("done is %v after the rest was CANCELLED, want 5 — cancelling "+
			"work is not delivering it", after.Figures.Done)
	}

	// AND THE BREAKDOWN SUMS TO THE TOTAL, because the two are rendered
	// beside each other and one that did not is a bug somebody files.
	var byAssignee float64
	for _, a := range after.ByAssignee {
		byAssignee += a.Done
	}
	if byAssignee != after.Figures.Done {
		t.Fatalf("by_assignee sums to %v where the sprint's done is %v — the "+
			"two are one predicate or they drift", byAssignee, after.Figures.Done)
	}
}

// A DELIVERY OUTSIDE THE WINDOW IS NOT THIS SPRINT'S.
//
// The window ends where the sprint did: a closed sprint stops at its close,
// not at whenever the report is run. Without the bound, work finished after a
// sprint closed would keep raising its velocity for ever.
func TestASprintsWindowEndsWhereTheSprintDid(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	// A sprint that closed BEFORE this test's records were written, so
	// every effective instant falls after its close.
	closed := base.Add(-time.Hour)
	seedSprintWindow(t, r, 1, tracker.SprintClosed,
		base.Add(-14*24*time.Hour), closed, &closed)
	one := 1
	pointedTask(t, r, "late", 8, &one, "ada")

	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-late", "late", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("finish a task: %v", err)
	}
	r.drain()

	got := r.sprints(tracker.SprintQuery{Project: "ENG"}, base).Sprints[0]
	if got.Figures.Done != 0 {
		t.Fatalf("done is %v for work delivered AFTER the sprint closed, want "+
			"0 — a window that ran to now would keep raising a closed "+
			"sprint's velocity for ever", got.Figures.Done)
	}
	if got.ClosedAt == nil || !got.ClosedAt.Equal(closed) {
		t.Fatalf("closed_at is %v, want the close instant", got.ClosedAt)
	}
	// AND A CLOSED SPRINT HAS NO REMAINDER — days_remaining is absent
	// rather than negative.
	if got.DaysRemaining != nil {
		t.Fatalf("days_remaining is %v on a CLOSED sprint, want absent",
			*got.DaysRemaining)
	}
}

// A CLOSED SPRINT WITH NO SPILLOVER DECISION IS PENDING, and that is a state
// rather than an absence somebody has to interpret.
func TestAnUnsettledSpilloverIsReportedPending(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	closed := base.Add(-time.Hour)
	seedSprintWindow(t, r, 1, tracker.SprintClosed,
		base.Add(-14*24*time.Hour), closed, &closed)
	one := 1
	pointedTask(t, r, "over", 5, &one, "ada")

	got := r.sprints(tracker.SprintQuery{Project: "ENG"}, base).Sprints[0]
	if !got.RolloverPending {
		t.Fatalf("a closed sprint with no rollover_to is not reported pending "+
			"— it is the one thing on this answer a lead has to act on: %+v", got)
	}
	// AND ITS OPEN STAYS ARE OPEN AFTER THE CLOSE. They carry no
	// `rolled_to` because nobody has decided where they go, and leaving
	// them out would report a closed sprint whose remaining work vanished.
	if got.Figures.OpenAfterClose != 5 {
		t.Fatalf("open_after_close is %v, want the 5 points nobody has settled",
			got.Figures.OpenAfterClose)
	}

	// AND THE PROJECT LISTING CARRIES IT, because that is where a lead
	// looks before opening one project.
	listing := r.projects(tracker.ProjectQuery{})
	sprints := listing.Projects[0].Sprints
	if sprints == nil || len(sprints.PendingSpillovers) != 1 ||
		sprints.PendingSpillovers[0] != 1 {
		t.Fatalf("the project row's pending spillovers are %+v, want [1]", sprints)
	}
}

// VELOCITY IS THE MEAN OVER THE CLOSED SPRINTS, and absent where none has
// closed — a team that has not finished a sprint has no velocity, and zero
// reads as a team that delivers nothing.
func TestVelocityAveragesTheClosedSprintsOnly(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	seedSprintWindow(t, r, 1, tracker.SprintActive,
		base.Add(-time.Hour), base.Add(13*24*time.Hour), nil)

	if got := r.sprints(tracker.SprintQuery{Project: "ENG"}, base); got.VelocityAvg != nil {
		t.Fatalf("velocity_avg is %v with no sprint closed, want absent",
			*got.VelocityAvg)
	}

	// A WINDOW OUTSIDE 3..10 IS REFUSED naming the range, rather than
	// silently clamped: a caller asking for 40 sprints wants to know it
	// got 10.
	_, err := r.reader.Sprints(t.Context(), tracker.SprintQuery{
		Project: "ENG", Sprints: 40, Level: statelog.ReadStale,
	}, wednesday)
	if err == nil || !strings.Contains(err.Error(), "3..10") {
		t.Fatalf("sprints=40 answered %v, want a refusal naming the range", err)
	}

	// AND A SPRINT REPORT NAMES A PROJECT. A company-wide one would be
	// summing two teams' unrelated cadences.
	if _, err := r.reader.Sprints(t.Context(), tracker.SprintQuery{
		Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Fatal("a sprint report with no project answered")
	}
	if _, err := r.reader.Sprints(t.Context(), tracker.SprintQuery{
		Project: "NOPE", Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Fatal("a sprint report for an unknown project answered — a reader " +
			"that answered empty would say the project has no sprints")
	}
}

// THE WINDOW IS THE LAST N, and what falls below it is NAMED rather than
// silently truncated.
func TestASprintWindowNamesWhatItDropped(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	for n := 1; n <= 8; n++ {
		closed := base.Add(time.Duration(-30+n) * 24 * time.Hour)
		seedSprintWindow(t, r, n, tracker.SprintClosed,
			closed.Add(-14*24*time.Hour), closed, &closed)
	}

	listing := r.sprints(tracker.SprintQuery{Project: "ENG"}, base)
	if len(listing.Sprints) != tracker.SprintWindowDefault {
		t.Fatalf("the default window covers %d sprints, want %d",
			len(listing.Sprints), tracker.SprintWindowDefault)
	}
	// OLDEST FIRST, which is how every burndown, velocity chart and sprint
	// list reads — the LIMIT takes the newest and the answer reverses them.
	if listing.Sprints[0].Number != 4 || listing.Sprints[4].Number != 8 {
		t.Fatalf("the window covers %d..%d, want the LAST five in sprint order",
			listing.Sprints[0].Number, listing.Sprints[4].Number)
	}
	if listing.EarlierSprintsDropped != 3 {
		t.Fatalf("earlier_sprints_dropped is %d, want 3 — a bound that is not "+
			"named reads as a company with only five sprints",
			listing.EarlierSprintsDropped)
	}
	if got := r.sprints(tracker.SprintQuery{Project: "ENG", Sprints: 3}, base); len(got.Sprints) != 3 ||
		got.EarlierSprintsDropped != 5 {
		t.Fatalf("sprints=3 covers %d with %d dropped, want 3 and 5",
			len(got.Sprints), got.EarlierSprintsDropped)
	}
	// AND ASKING FOR ONE SPRINT DROPS NOTHING, because a single-sprint
	// report is not a window.
	if got := r.sprints(tracker.SprintQuery{Project: "ENG", Number: 2}, base); len(got.Sprints) != 1 ||
		got.Sprints[0].Number != 2 || got.EarlierSprintsDropped != 0 {
		t.Fatalf("a single-sprint report is %+v with %d dropped, want sprint 2 "+
			"and nothing dropped", got.Sprints, got.EarlierSprintsDropped)
	}
}

// A CAPACITY IS COMPARED AGAINST WHAT SOMEBODY HOLDS, and an UNDECLARED one is
// absent rather than zero — which would render every assignee permanently over.
func TestASprintCapacityIsAbsentUntilItIsDeclared(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	seedSprintWindow(t, r, 1, tracker.SprintActive,
		base.Add(-time.Hour), base.Add(13*24*time.Hour), nil)
	one := 1
	pointedTask(t, r, "a", 8, &one, "ada")
	pointedTask(t, r, "b", 2, &one, "bo")

	before := r.sprints(tracker.SprintQuery{Project: "ENG"}, base).Sprints[0]
	for _, a := range before.ByAssignee {
		if a.Capacity != nil || a.OverCapacity {
			t.Fatalf("%s carries capacity %v over=%v with none declared — an "+
				"unset capacity is not a capacity of zero", a.Handle,
				a.Capacity, a.OverCapacity)
		}
	}

	seedProject(t, r, tracker.Project{Key: "ENG", Name: "Engineering",
		Sprints: &tracker.SprintPolicy{
			LengthDays: 14, Next: 2, Measure: tracker.MeasurePoints,
			Capacity: map[string]tracker.Capacity{
				"ada": {Points: 5}, "bo": {Points: 5},
			},
		}})

	after := byHandle(r.sprints(tracker.SprintQuery{Project: "ENG"}, base).Sprints[0])
	if got := after["ada"]; got.Capacity == nil || *got.Capacity != 5 || !got.OverCapacity {
		t.Fatalf("ada holds %v of a capacity of 5 and is over=%v, want over — "+
			"she is carrying 8", got.Total, got.OverCapacity)
	}
	if got := after["bo"]; got.Capacity == nil || got.OverCapacity {
		t.Fatalf("bo holds %v of a capacity of 5 and is over=%v, want under",
			got.Total, got.OverCapacity)
	}
}

// THE MEASURE IS THE PROJECT'S, and the answer says which — a figure reported
// in the wrong column is worse than no figure.
func TestASprintReportsInTheProjectsOwnMeasure(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "ENG", Name: "Engineering",
		Sprints: &tracker.SprintPolicy{LengthDays: 14, Next: 2,
			Measure: tracker.MeasureEstimate}})
	base := time.Now().UTC().Truncate(time.Microsecond)
	seedSprintWindow(t, r, 1, tracker.SprintActive,
		base.Add(-time.Hour), base.Add(13*24*time.Hour), nil)

	one := 1
	task := newTask("e")
	task.Key = "ENG-e"
	task.Points = 13
	task.EstimateMinutes = 90
	task.Sprint = &one
	if _, err := r.writer.CreateTask(t.Context(), "op-e", task, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	listing := r.sprints(tracker.SprintQuery{Project: "ENG"}, base)
	if listing.Measure != tracker.MeasureEstimate {
		t.Fatalf("the listing's measure is %q, want estimate_min", listing.Measure)
	}
	if got := listing.Sprints[0].Figures.Added; got != 90 {
		t.Fatalf("added is %v, want the 90 MINUTES — a project running on "+
			"estimates reported 13 points would be reporting the wrong column",
			got)
	}
}

func byHandle(row tracker.SprintRow) map[string]tracker.AssigneeFigures {
	out := make(map[string]tracker.AssigneeFigures, len(row.ByAssignee))
	for _, a := range row.ByAssignee {
		out[a.Handle] = a
	}
	return out
}
