package builtin_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A SEAT THAT NAMES NO PROJECT MEANS ITS OWN, on every one of these.
//
// Three tools fall back to it and each one used to be a separate chance to
// get it wrong: a describe that answered about a different project from the
// one a create files into teaches a model the wrong vocabulary for the
// container it is writing to, and the model has no way to notice.
func TestTheProjectToolsFallBackToTheSeatsOwnProject(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as,
		DefaultProject: func(string) string { return "ENG" },
	})

	if got := callWork(t, reg, tracker.DescribeProjectTool, map[string]any{}); got.Failed {
		t.Fatalf("describe_project with no project failed: %q", got.Output)
	}
	if trk.detailQuery.Project != "ENG" {
		t.Errorf("describe_project asked about %q, want the seat's own project",
			trk.detailQuery.Project)
	}
	if got := callWork(t, reg, tracker.SprintReportTool, map[string]any{}); got.Failed {
		t.Fatalf("sprint_report with no project failed: %q", got.Output)
	}
	if trk.sprintQuery.Project != "ENG" {
		t.Errorf("sprint_report asked about %q, want the seat's own project",
			trk.sprintQuery.Project)
	}

	// AND A SEAT WHOSE UNIT OWNS NONE IS REFUSED NAMING THE LOOKUP, never
	// answered about an empty key: an empty project is not a project, and
	// a reader handed one would answer "no such project ''".
	unowned := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	got := callWork(t, unowned, tracker.DescribeProjectTool, map[string]any{})
	if !got.Failed || !strings.Contains(got.Output, "list_projects") {
		t.Errorf("describe_project with no project and no default gave %q, "+
			"want a refusal naming the lookup", got.Output)
	}
}

// EVERY ONE OF THESE READS AT THE SEAT SURFACE'S OWN DEFAULT.
//
// A turn must see its own writes: a seat that files into a project and then
// describes it would otherwise read a copy from before its own create, and a
// model that cannot see its own write files it again. The level is asserted
// against [statelog.DefaultReadLevel] rather than a literal, because a literal
// is what let twenty-one call sites drift to a level nothing populated: a
// `session` read with no position is this node's committed prefix wearing a
// stronger name, which is exactly the stale answer the comment above forbids.
func TestTheProjectToolsReadAtTheSeatDefault(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as,
		DefaultProject: func(string) string { return "ENG" },
	})
	callWork(t, reg, tracker.ListProjectsTool, map[string]any{})
	callWork(t, reg, tracker.DescribeProjectTool, map[string]any{})
	callWork(t, reg, tracker.SprintReportTool, map[string]any{})

	want := statelog.DefaultReadLevel(statelog.SurfaceSeat)
	for name, level := range map[string]statelog.ReadLevel{
		tracker.ListProjectsTool:    trk.projectQuery.Level,
		tracker.DescribeProjectTool: trk.detailQuery.Level,
		tracker.SprintReportTool:    trk.sprintQuery.Level,
	} {
		if level != want {
			t.Errorf("%s read at %q, want %q — a turn has to see its own "+
				"writes", name, level, want)
		}
	}
}

// THE ARGUMENTS REACH THE QUERY. Each one is a filter a model typed, and a
// tool that accepted it and then dropped it answers a question nobody asked.
func TestTheProjectToolsCarryTheirArguments(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	callWork(t, reg, tracker.ListProjectsTool, map[string]any{
		"q": "platform", "unit": "Engineering", "archived": true, "limit": 12,
	})
	q := trk.projectQuery
	if q.Q != "platform" || q.Unit != "Engineering" || !q.Archived || q.Limit != 12 {
		t.Errorf("list_projects built %+v, want every argument carried", q)
	}

	callWork(t, reg, tracker.DescribeProjectTool, map[string]any{
		"project": "ops", "for_type": "bug",
	})
	if trk.detailQuery.Project != "ops" || trk.detailQuery.ForType != "bug" {
		t.Errorf("describe_project built %+v, want the project and the type",
			trk.detailQuery)
	}

	callWork(t, reg, tracker.SprintReportTool, map[string]any{
		"project": "OPS", "sprint": 4, "sprints": 7,
	})
	if trk.sprintQuery.Project != "OPS" || trk.sprintQuery.Number != 4 ||
		trk.sprintQuery.Sprints != 7 {
		t.Errorf("sprint_report built %+v, want the project, the sprint and "+
			"the window", trk.sprintQuery)
	}
}

// THE UNIT SEAM TRAVELS. Resolving a project's chart-owned unit is a READ-time
// join against the epoch's org, and a tool that did not pass its resolver
// would report every project orphaned from a chart that in fact names it.
func TestTheProjectToolsPassTheChart(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	units := stubUnits{}
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Units: units,
		DefaultProject: func(string) string { return "ENG" },
	})
	callWork(t, reg, tracker.ListProjectsTool, map[string]any{})
	if trk.projectQuery.Units == nil {
		t.Error("list_projects passed no chart, so every project's unit reads " +
			"as one the org no longer has")
	}
	callWork(t, reg, tracker.DescribeProjectTool, map[string]any{})
	if trk.detailQuery.Units == nil {
		t.Error("describe_project passed no chart")
	}
}

// A READ FAILURE IS A TOOL FAILURE, never an empty answer: "this company has
// no projects" is a conclusion a model acts on, by filing into one it invents.
func TestAProjectReadFailureIsReported(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.readErr = errors.New("this node has not caught up")
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as,
		DefaultProject: func(string) string { return "ENG" },
	})
	for _, name := range []string{
		tracker.ListProjectsTool, tracker.DescribeProjectTool,
		tracker.SprintReportTool,
	} {
		if got := callWork(t, reg, name, map[string]any{}); !got.Failed {
			t.Errorf("%s answered %q on a read failure, want a failure — an "+
				"empty answer reads as a company with no projects",
				name, got.Output)
		}
	}
}

type stubUnits struct{}

func (stubUnits) ResolveUnit(string) (string, tracker.LeadRef, bool) {
	return "Platform", tracker.LeadRef{Handle: "ada", Kind: tracker.AuthorAgent}, true
}

// MY_WORK TAKES NO HANDLE, ever.
//
// A model that could name whose day to read could read anybody's — which is a
// colleague's priorities, their inbox and the questions they owe, handed to an
// agent nobody asked. The handle is the turn's own seat and comes from the
// immutable turn context the tool surface bound.
func TestMyWorkIsAlwaysTheTurnsOwnSeat(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	callWork(t, reg, tracker.MyWorkTool, map[string]any{"handle": "somebody-else"})
	if trk.myWorkQuery.Handle != "eng" {
		t.Errorf("my_work read %q's day, want the turn's own seat — a tool "+
			"that took a handle would hand one agent a colleague's queue",
			trk.myWorkQuery.Handle)
	}
	entry, ok := reg.Lookup(tracker.MyWorkTool)
	if !ok {
		t.Fatal("my_work is not registered")
	}
	params, _ := entry.Tool.Parameters()["properties"].(map[string]any)
	if len(params) != 0 {
		t.Errorf("my_work declares %v — a parameter a model can set is a "+
			"parameter it will set", params)
	}
	if want := statelog.DefaultReadLevel(statelog.SurfaceSeat); trk.myWorkQuery.Level != want {
		t.Errorf("my_work read at %q, want %q — a turn opens on this "+
			"answer and must see its own last turn's writes",
			trk.myWorkQuery.Level, want)
	}
}

// THE FEED FALLS BACK TO THE SEAT'S OWN PROJECT, never to the whole company.
//
// A model asking "what has been going on" means its own work, and a
// company-wide feed is the one answer that is both expensive and almost never
// what was meant.
func TestTaskActivityFallsBackToTheSeatsOwnProject(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as,
		DefaultProject: func(string) string { return "ENG" },
	})

	callWork(t, reg, tracker.TaskActivityTool, map[string]any{})
	if trk.activityQuery.Project != "ENG" || trk.activityQuery.Workspace {
		t.Errorf("task_activity asked for %+v, want the seat's own project",
			trk.activityQuery)
	}

	// AND A NAMED TASK WINS, because it is the narrower question.
	callWork(t, reg, tracker.TaskActivityTool, map[string]any{
		"task": "ENG-1", "kinds": "status,assignee", "actor": "ada", "limit": 5,
	})
	q := trk.activityQuery
	if q.Task != "ENG-1" || q.Actor != "ada" || q.Limit != 5 || len(q.Kinds) != 2 {
		t.Errorf("task_activity built %+v, want every argument carried", q)
	}

	// A `since` THAT IS NEITHER SHAPE IS REFUSED naming both, rather than
	// silently dropped — a bound the caller believed in and that never
	// reached the query answers a different question.
	got := callWork(t, reg, tracker.TaskActivityTool, map[string]any{
		"task": "ENG-1", "since": "last tuesday",
	})
	if !got.Failed || !strings.Contains(got.Output, "RFC3339") {
		t.Errorf("an unparseable `since` gave %q, want a refusal naming the "+
			"two shapes", got.Output)
	}
}
