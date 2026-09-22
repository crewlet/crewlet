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

	want := statelog.DefaultReadLevel(statelog.SurfaceSeat)
	for name, level := range map[string]statelog.ReadLevel{
		tracker.ListProjectsTool:    trk.projectQuery.Level,
		tracker.DescribeProjectTool: trk.detailQuery.Level,
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
		"q": "platform", "unit": "Engineering", "archived": "only",
		"sort": "-open", "limit": 12,
	})
	q := trk.projectQuery
	if q.Q != "platform" || q.Unit != "Engineering" || q.Limit != 12 {
		t.Errorf("list_projects built %+v, want every argument carried", q)
	}
	// THE SAME GRAMMAR THE SCREEN'S IS. `archived=only` SELECTS the
	// retired ones, and a tool that read it as the two-valued flag it
	// replaced would answer a model about both sets while the model's own
	// question was about one.
	if q.Archived != tracker.ArchivedOnly {
		t.Errorf("archived=only reached the query as %q, want %q",
			q.Archived, tracker.ArchivedOnly)
	}
	if q.Sort != tracker.ProjectSortOpen || !q.Descending {
		t.Errorf("sort=-open reached the query as %q/%v, want open descending "+
			"— a seat's page is fifty of the company's projects, so which "+
			"fifty is what the ordering decides", q.Sort, q.Descending)
	}

	// AN ABSENT `archived` IS THE LIVE SET, resolved at the parse so the
	// read never sees a query that named no set at all.
	trk.projectQuery = tracker.ProjectQuery{}
	callWork(t, reg, tracker.ListProjectsTool, map[string]any{})
	if trk.projectQuery.Archived != tracker.ArchivedExclude {
		t.Errorf("an absent archived reached the query as %q, want %q",
			trk.projectQuery.Archived, tracker.ArchivedExclude)
	}

	// AND A VALUE THAT IS NOT ONE IS A TOOL FAILURE NAMING IT, never a
	// silent fall back to the default: a model told nothing learns nothing
	// and asks the same wrong question again.
	res := callWork(t, reg, tracker.ListProjectsTool, map[string]any{
		"archived": "yes",
	})
	if !res.Failed || !strings.Contains(res.Output, "yes") {
		t.Errorf("archived=yes answered %+v, want a failure quoting the value",
			res)
	}

	callWork(t, reg, tracker.DescribeProjectTool, map[string]any{
		"project": "ops", "for_type": "bug",
	})
	if trk.detailQuery.Project != "ops" || trk.detailQuery.ForType != "bug" {
		t.Errorf("describe_project built %+v, want the project and the type",
			trk.detailQuery)
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
	} {
		if got := callWork(t, reg, name, map[string]any{}); !got.Failed {
			t.Errorf("%s answered %q on a read failure, want a failure — an "+
				"empty answer reads as a company with no projects",
				name, got.Output)
		}
	}
}

// stubUnits is a test chart holding ONE unit, which answers to its id or its
// name in any case — the contract [tracker.Units] states and the engine's own
// resolver keeps.
type stubUnits struct{}

func (stubUnits) ResolveUnit(ref string) (tracker.ChartUnit, bool) {
	ref = strings.TrimSpace(ref)
	if !strings.EqualFold(ref, "plat") && !strings.EqualFold(ref, "Platform") {
		return tracker.ChartUnit{}, false
	}
	return tracker.ChartUnit{
		Key: "plat", Name: "Platform",
		Lead: tracker.LeadRef{Handle: "ada", Kind: tracker.AuthorAgent},
	}, true
}

func (s stubUnits) AllUnits() []tracker.ChartUnit {
	unit, _ := s.ResolveUnit("plat")
	return []tracker.ChartUnit{unit}
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
	if trk.myWorkQuery.Who.Handle != "eng" {
		t.Errorf("my_work read %q's day, want the turn's own seat — a tool "+
			"that took a handle would hand one agent a colleague's queue",
			trk.myWorkQuery.Who.Handle)
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
