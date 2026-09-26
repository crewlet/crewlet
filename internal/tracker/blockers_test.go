package tracker_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// row returns one task's board row from a container-wide listing.
func row(t *testing.T, r *roundTrip, id string) tracker.TaskRow {
	t.Helper()
	for _, got := range r.ask(map[string]any{"container": "project:ENG"}).Rows {
		if got.ID == id {
			return got
		}
	}
	t.Fatalf("no row for %s in the project's own listing", id)
	return tracker.TaskRow{}
}

// blockerIDs is what a row says it waits on, in the order it said it.
func blockerIDs(got tracker.TaskRow) []string {
	out := []string{}
	for _, edge := range got.WaitingOn {
		out = append(out, edge.ID)
	}
	return out
}

// THE ROW CARRIES WHICH TASK HOLDS IT UP, not only that something does.
//
// `blocked` is one bit, and a bit cannot be drawn as a relation: a renderer
// showing two bars on a date axis has to know WHICH of the rows it holds is
// the blocker, and the only other way to learn that was a single-task read per
// bar — fifty reads to draw fifty rows.
func TestABoardRowNamesTheTasksItWaitsOn(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")

	if _, err := r.writer.Depend(t.Context(), "op-depend", tracker.DependencyChange{
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
	// AND THE BLOCKER'S OWN ROW CARRIES NOTHING: the edge is authored on
	// one end, and a row that listed the other direction too would make
	// every arrow appear twice.
	if got := blockerIDs(row(t, r, "blk")); len(got) != 0 {
		t.Errorf("the blocker's row waits on %v, want nothing — the edge is "+
			"authored on the dependent", got)
	}
}

// THE FLAG IS THE EDGES, so no screen can show one without the other.
//
// `blocked` comes from an EXISTS in the row's own SELECT and `waiting_on` from
// a second statement over two more tables. Two computations of one fact is how
// a badge ends up beside no arrows, or an arrow beside no badge — the exact
// contradiction [TaskRow.Overdue] exists to prevent for dates.
func TestTheBlockedFlagIsTheEdgesItCarries(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")
	filedTask(t, r, "free")

	if _, err := r.writer.Depend(t.Context(), "op-depend", tracker.DependencyChange{
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
				func(e tracker.Blocker) bool { return e.Open })
			if got.Blocked != open {
				t.Errorf("%s: %s is blocked=%t and carries %+v — the flag and "+
					"the edges are two computations of one fact",
					when, got.Key, got.Blocked, got.WaitingOn)
			}
		}
	}
	check("with the blocker open")

	// AND AFTER THE BLOCKER FINISHES, which is the transition that moves
	// the flag: the edge stays, its state changes, and both have to move
	// together.
	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "blk", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done},
		tracker.ChangeStatus, nil); err != nil {
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

	if _, err := r.writer.Depend(t.Context(), "op-depend", tracker.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()

	// THE KEY IS THE WRITER'S, never the caller's: `CreateTask` mints it
	// from the project's own counter, so what a test asked to call a task
	// is not what `key=` matches.
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

// THE TIMELINE IS A SHAPE EVERY CONTAINER HAS, and it arrives ordered by when
// work STARTS.
//
// A date axis whose rows come in rank order draws a staircase nobody can read,
// and re-sorting in the client would make the ORDER of a page depend on which
// rows the page happened to contain — the one arrangement a paged answer
// cannot be fixed up after the fact.
func TestEveryContainerOffersATimelineOrderedByWhenWorkStarts(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "one")

	strip := r.strip(
		tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"}, "ada")
	var timeline *tracker.ViewRow
	for i, view := range strip.Views {
		if view.Type == tracker.ViewTimeline {
			timeline = &strip.Views[i]
		}
	}
	if timeline == nil {
		shapes := []tracker.ViewType{}
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
	// AND IT IS A SHAPE THE WRITE SIDE ACCEPTS, so a person can save their
	// own filters as one. A tab nothing could author would be a rendering
	// with exactly one instance for ever.
	if !tracker.ViewTimeline.Valid() {
		t.Error("a view naming the timeline shape is refused at the write — " +
			"the tab exists and nobody can save one")
	}
}

// A WRITE TO A BLOCKER IS PROBED WHERE ITS DEPENDENT IS FILED.
//
// Every apply of a blocker rewrites rows keyed on the tasks waiting on it, so
// its record claims each dependent — and a dependent in ANOTHER project is
// filed there, not under the blocker's. The scope named every dependent under
// the blocker's own project, a path no record about it is filed under, so a
// record this node could not decode about a cross-project dependent never
// stopped the write that rewrites that dependent's row.
func TestAWriteToABlockerIsProbedWhereItsDependentIsFiled(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker := r.createTask("the blocker")
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations"})
	waiting := newTask("t-waits-from-ops")
	waiting.Project, waiting.Key, waiting.Title = "OPS", "", "waits across projects"
	if _, err := r.writer.CreateTask(t.Context(), "op-"+waiting.ID, waiting, nil); err != nil {
		t.Fatalf("file the dependent: %v", err)
	}
	r.drain()
	if _, err := r.writer.Depend(t.Context(), "op-depend", tracker.DependencyChange{
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
		tracker.NoIfMatch, tracker.TaskPatch{Assignee: strptr("ana")}, tracker.ChangeAssignee, nil)
	var unavailable *statelog.Unavailable
	if !errors.As(err, &unavailable) || unavailable.Reason != statelog.ReasonDeferred {
		t.Fatalf("a write rewriting a dependent a deferred record covers = %v, "+
			"want it refused as deferred", err)
	}
}
