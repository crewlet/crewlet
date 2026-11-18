package worklet_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// row returns one task's board row from a container-wide listing.
func row(t *testing.T, r *roundTrip, id string) worklet.TaskRow {
	t.Helper()
	for _, got := range r.ask(map[string]any{"container": "project:ENG"}).Rows {
		if got.ID == id {
			return got
		}
	}
	t.Fatalf("no row for %s in the project's own listing", id)
	return worklet.TaskRow{}
}

// blockerIDs is what a row says it waits on, in the order it said it.
func blockerIDs(got worklet.TaskRow) []string {
	out := []string{}
	for _, edge := range got.WaitingOn {
		out = append(out, edge.ID)
	}
	return out
}

// THE ROW CARRIES WHICH TASK HOLDS IT UP, not only that something does:
// `blocked` is one bit, and a renderer drawing bars on a date axis must know
// which row is the blocker without a single-task read per bar.
func TestABoardRowNamesTheTasksItWaitsOn(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")

	if _, err := r.writer.Depend(t.Context(), "op-depend", worklet.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()

	dep := row(t, r, "dep")
	if got := blockerIDs(dep); !slices.Equal(got, []string{"blk"}) {
		t.Fatalf("the dependent's row waits on %v, want the one blocker — "+
			"without the id on the row a timeline can draw no edge", got)
	}
	if !dep.WaitingOn[0].Open {
		t.Error("the edge reports its blocker closed while the blocker is " +
			"open — a renderer would draw a settled dependency")
	}
	// The blocker's own row carries nothing: the edge is authored on the
	// dependent, and listing both directions would draw every arrow twice.
	if got := blockerIDs(row(t, r, "blk")); len(got) != 0 {
		t.Errorf("the blocker's row waits on %v, want nothing — the edge is "+
			"authored on the dependent", got)
	}
}

// THE FLAG IS THE EDGES, so no screen can show one without the other.
// `blocked` comes from an EXISTS in the row's SELECT and `waiting_on` from a
// second statement: two computations of one fact, the contradiction
// [TaskRow.Overdue] prevents for dates.
func TestTheBlockedFlagIsTheEdgesItCarries(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")
	filedTask(t, r, "free")

	if _, err := r.writer.Depend(t.Context(), "op-depend", worklet.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()

	check := func(when string) {
		t.Helper()
		for _, got := range r.ask(map[string]any{
			"container": "project:ENG", "show_closed": "true",
		}).Rows {
			open := slices.ContainsFunc(got.WaitingOn,
				func(e worklet.Blocker) bool { return e.Open })
			if got.Blocked != open {
				t.Errorf("%s: %s is blocked=%t and carries %+v — the flag and "+
					"the edges are two computations of one fact",
					when, got.Key, got.Blocked, got.WaitingOn)
			}
		}
	}
	check("with the blocker open")

	// After the blocker finishes, the transition that moves the flag: the
	// edge stays, its state changes, and both have to move together.
	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "blk", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &done},
		worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("close the blocker: %v", err)
	}
	r.drain()
	check("with the blocker finished")

	dep := row(t, r, "dep")
	if len(dep.WaitingOn) != 1 {
		t.Fatalf("the cleared edge carries %+v, want the one it always had — "+
			"a timeline that dropped it would redraw its own history every "+
			"time a blocker closed", dep.WaitingOn)
	}
	if dep.WaitingOn[0].Open {
		t.Error("the edge still reports its blocker open after it finished")
	}
}

// A BLOCKER THE FILTER EXCLUDED IS AN ID WITH NO ROW, which is the honest
// answer: the edge exists and this page cannot draw it. The flag is what still
// says something is holding the task up.
func TestAnEdgeSurvivesItsBlockerLeavingThePage(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")

	if _, err := r.writer.Depend(t.Context(), "op-depend", worklet.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()

	// The key is the writer's, never the caller's: CreateTask mints it from
	// the project's own counter.
	answer := r.ask(map[string]any{
		"container": "project:ENG", "key": row(t, r, "dep").Key,
	})
	if len(answer.Rows) != 1 {
		t.Fatalf("the narrowed listing answers %d rows, want the one asked for",
			len(answer.Rows))
	}
	if got := blockerIDs(answer.Rows[0]); !slices.Equal(got, []string{"blk"}) {
		t.Fatalf("a page holding only the dependent waits on %v, want the "+
			"blocker's id all the same — an edge is not a property of who "+
			"else the caller asked for", got)
	}
}

// THE TIMELINE IS A SHAPE EVERY CONTAINER HAS, ordered by when work STARTS:
// rank order draws an unreadable staircase on a date axis, and a client re-sort
// would make a page's order depend on which rows it holds, which a paged
// answer cannot fix up after the fact.
func TestEveryContainerOffersATimelineOrderedByWhenWorkStarts(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "one")

	strip := r.strip(
		worklet.Container{Kind: worklet.ContainerProject, ID: "ENG"}, "ada")
	var timeline *worklet.ViewRow
	for i, view := range strip.Views {
		if view.Type == worklet.ViewTimeline {
			timeline = &strip.Views[i]
		}
	}
	if timeline == nil {
		shapes := []worklet.ViewType{}
		for _, view := range strip.Views {
			shapes = append(shapes, view.Type)
		}
		t.Fatalf("the strip offers %v and no timeline", shapes)
	}
	if !timeline.Builtin {
		t.Error("the timeline is a saved view rather than one every container " +
			"has — a company that never made one would have no date axis")
	}
	if got := timeline.Params["sort"]; got != "start" {
		t.Errorf("the timeline arrives sorted by %q, want the axis it draws "+
			"against", got)
	}
	// And the write side accepts the shape, so a person can save their own
	// filters as one rather than the tab having exactly one instance.
	if !worklet.ViewTimeline.Valid() {
		t.Error("a view naming the timeline shape is refused at the write — " +
			"the tab exists and nobody can save one")
	}
}

// A WRITE TO A BLOCKER IS PROBED WHERE ITS DEPENDENT IS FILED. Each apply
// of a blocker rewrites rows keyed on its dependents, so its record claims
// each one under the project it is filed in, not the blocker's: otherwise a
// record this node cannot decode about a cross-project dependent would not
// stop the write that rewrites that dependent's row.
func TestAWriteToABlockerIsProbedWhereItsDependentIsFiled(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker := r.createTask("the blocker")
	seedProject(t, r, worklet.Project{Key: "OPS", Name: "Operations"})
	waiting := newTask("t-waits-from-ops")
	waiting.Project, waiting.Key, waiting.Title = "OPS", "", "waits across projects"
	if _, err := r.writer.CreateTask(t.Context(), "op-"+waiting.ID, waiting, nil); err != nil {
		t.Fatalf("file the dependent: %v", err)
	}
	r.drain()
	if _, err := r.writer.Depend(t.Context(), "op-depend", worklet.DependencyChange{
		Task: waiting.ID, Project: "OPS", WaitingOnAdd: []string{blocker.ID},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("depend across projects: %v", err)
	}
	r.drain()
	if held := oneTask(t, r, blocker.ID); !slices.Contains(held.Dependents, waiting.ID) {
		t.Fatalf("the blocker lists dependents %v, so its writes claim no "+
			"dependent and this case is about nothing", held.Dependents)
	}
	r.deferRecordOn(waiting.ID, "OPS")

	_, err := r.writer.UpdateTask(t.Context(), "op-edit-blocker", blocker.ID, blocker.Project,
		worklet.NoIfMatch, worklet.TaskPatch{Assignee: strptr("ana")}, worklet.ChangeAssignee, nil)
	var unavailable *statelog.Unavailable
	if !errors.As(err, &unavailable) || unavailable.Reason != statelog.ReasonDeferred {
		t.Fatalf("a write rewriting a dependent a deferred record covers = %v, "+
			"want it refused as deferred", err)
	}
}

// AN EDGE NAMES ITS BLOCKER BY KEY, so a page holding no row for the blocker
// (a project's list, for one in another project) can still say which task
// holds it up. Asked for the dependent alone, the answer still names it.
func TestAnEdgeNamesItsBlockerByKeyWithoutItsRow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")
	if _, err := r.writer.Depend(t.Context(), "op-depend", worklet.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()
	want := r.task(t, "blk").Task.Key
	if want == "" {
		t.Fatal("the blocker has no key, so this case tests nothing")
	}
	dep := row(t, r, "dep")
	if len(dep.WaitingOn) != 1 || dep.WaitingOn[0].Key != want {
		t.Fatalf("the dependent's edge is %+v, want it to name its blocker as %s", dep.WaitingOn, want)
	}
}
