package builtin_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A SEAT THAT NAMES NO PROJECT MEANS ITS OWN, on every one of these: a
// describe about a different project from the one a create files into teaches
// a model the wrong vocabulary, and it has no way to notice.
func TestTheProjectToolsFallBackToTheSeatsOwnProject(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as,
		DefaultProject: func(string) string { return "ENG" },
	})

	if got := callWork(t, reg, worklet.DescribeProjectTool, map[string]any{}); got.Failed {
		t.Fatalf("describe_project with no project failed: %q", got.Output)
	}
	if trk.detailQuery.Project != "ENG" {
		t.Errorf("describe_project asked about %q, want the seat's own project",
			trk.detailQuery.Project)
	}

	// AND A SEAT WHOSE UNIT OWNS NONE IS REFUSED NAMING THE LOOKUP, never
	// answered about an empty key ("no such project ''").
	unowned := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	got := callWork(t, unowned, worklet.DescribeProjectTool, map[string]any{})
	if !got.Failed || !strings.Contains(got.Output, "list_projects") {
		t.Errorf("describe_project with no project and no default gave %q, "+
			"want a refusal naming the lookup", got.Output)
	}
}

// EVERY ONE OF THESE READS AT THE SEAT SURFACE'S OWN DEFAULT, so a turn sees
// its own writes rather than filing one again. Asserted against
// [statelog.DefaultReadLevel], not a literal: literals drift to a level
// nothing populates (`session` with no position is this node's committed
// prefix under a stronger name).
func TestTheProjectToolsReadAtTheSeatDefault(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as,
		DefaultProject: func(string) string { return "ENG" },
	})
	callWork(t, reg, worklet.ListProjectsTool, map[string]any{})
	callWork(t, reg, worklet.DescribeProjectTool, map[string]any{})

	want := statelog.DefaultReadLevel(statelog.SurfaceSeat)
	for name, level := range map[string]statelog.ReadLevel{
		worklet.ListProjectsTool:    trk.projectQuery.Level,
		worklet.DescribeProjectTool: trk.detailQuery.Level,
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

	callWork(t, reg, worklet.ListProjectsTool, map[string]any{
		"q": "platform", "unit": "Engineering", "archived": "only",
		"sort": "-active", "limit": 12,
	})
	q := trk.projectQuery
	if q.Q != "platform" || q.Unit != "Engineering" || q.Limit != 12 {
		t.Errorf("list_projects built %+v, want every argument carried", q)
	}
	// THE SCREEN'S GRAMMAR: `archived=only` selects the retired ones alone,
	// never both sets as a two-valued flag would.
	if q.Archived != worklet.ArchivedOnly {
		t.Errorf("archived=only reached the query as %q, want %q",
			q.Archived, worklet.ArchivedOnly)
	}
	if q.Sort != worklet.ProjectSortActive || !q.Descending {
		t.Errorf("sort=-active reached the query as %q/%v, want active descending "+
			"— a seat's page is fifty of the company's projects, so which "+
			"fifty is what the ordering decides", q.Sort, q.Descending)
	}

	// AN ABSENT `archived` IS THE LIVE SET, resolved at the parse so the
	// read never sees a query that named no set at all.
	trk.projectQuery = worklet.ProjectQuery{}
	callWork(t, reg, worklet.ListProjectsTool, map[string]any{})
	if trk.projectQuery.Archived != worklet.ArchivedExclude {
		t.Errorf("an absent archived reached the query as %q, want %q",
			trk.projectQuery.Archived, worklet.ArchivedExclude)
	}

	// AND A VALUE THAT IS NOT ONE IS A TOOL FAILURE NAMING IT, never a
	// silent default: a model told nothing asks the same wrong question again.
	res := callWork(t, reg, worklet.ListProjectsTool, map[string]any{
		"archived": "yes",
	})
	if !res.Failed || !strings.Contains(res.Output, "yes") {
		t.Errorf("archived=yes answered %+v, want a failure quoting the value",
			res)
	}

	callWork(t, reg, worklet.DescribeProjectTool, map[string]any{
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
	callWork(t, reg, worklet.ListProjectsTool, map[string]any{})
	if trk.projectQuery.Units == nil {
		t.Error("list_projects passed no chart, so every project's unit reads " +
			"as one the org no longer has")
	}
	callWork(t, reg, worklet.DescribeProjectTool, map[string]any{})
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
		worklet.ListProjectsTool, worklet.DescribeProjectTool,
	} {
		if got := callWork(t, reg, name, map[string]any{}); !got.Failed {
			t.Errorf("%s answered %q on a read failure, want a failure — an "+
				"empty answer reads as a company with no projects",
				name, got.Output)
		}
	}
}

// stubUnits is a test chart holding ONE unit, which answers to its id or its
// name in any case, the contract [worklet.Units] states and the engine's own
// resolver keeps.
type stubUnits struct{}

func (stubUnits) ResolveUnit(ref string) (worklet.ChartUnit, bool) {
	ref = strings.TrimSpace(ref)
	if !strings.EqualFold(ref, "plat") && !strings.EqualFold(ref, "Platform") {
		return worklet.ChartUnit{}, false
	}
	return worklet.ChartUnit{
		Key: "plat", Name: "Platform",
		Lead: worklet.LeadRef{Handle: "ada", Kind: worklet.AuthorAgent},
	}, true
}

func (s stubUnits) AllUnits() []worklet.ChartUnit {
	unit, _ := s.ResolveUnit("plat")
	return []worklet.ChartUnit{unit}
}

// MY_WORK TAKES NO HANDLE, ever: one that could name whose day to read could
// read a colleague's priorities, inbox and owed questions. The handle is the
// turn's own seat, from the immutable turn context the tool surface bound.
func TestMyWorkIsAlwaysTheTurnsOwnSeat(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	// A HANDLE SENT ANYWAY IS REFUSED BY NAME and reads nobody's day, never
	// the caller's own as if it had asked for that.
	named := callWork(t, reg, worklet.MyWorkTool, map[string]any{"handle": "somebody-else"})
	if !named.Failed || !errors.Is(named.Cause, builtin.ErrUndeclaredArgument) {
		t.Errorf("my_work naming a handle answered %q (cause %v), want the "+
			"argument refused by name", named.Output, named.Cause)
	}
	if trk.myWorkQuery.Handle != "" {
		t.Fatalf("my_work naming a handle read %q's day", trk.myWorkQuery.Handle)
	}
	callWork(t, reg, worklet.MyWorkTool, map[string]any{})
	if trk.myWorkQuery.Handle != "eng" {
		t.Errorf("my_work read %q's day, want the turn's own seat — a tool "+
			"that took a handle would hand one agent a colleague's queue",
			trk.myWorkQuery.Handle)
	}
	entry, ok := reg.Lookup(worklet.MyWorkTool)
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

// THE FEED FALLS BACK TO THE SEAT'S OWN PROJECT, never the whole company: a
// company-wide feed is expensive and almost never what was meant.
func TestTaskActivityFallsBackToTheSeatsOwnProject(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as,
		DefaultProject: func(string) string { return "ENG" },
	})

	callWork(t, reg, worklet.TaskActivityTool, map[string]any{})
	if trk.activityQuery.Project != "ENG" || trk.activityQuery.Workspace {
		t.Errorf("task_activity asked for %+v, want the seat's own project",
			trk.activityQuery)
	}

	// AND A NAMED TASK WINS, because it is the narrower question.
	callWork(t, reg, worklet.TaskActivityTool, map[string]any{
		"task": "ENG-1", "kinds": "status,assignee", "actor": "ada", "limit": 5,
	})
	q := trk.activityQuery
	if q.Task != "ENG-1" || q.Actor != "ada" || q.Limit != 5 || len(q.Kinds) != 2 {
		t.Errorf("task_activity built %+v, want every argument carried", q)
	}

	// A `since` THAT IS NEITHER SHAPE IS REFUSED naming both, never dropped:
	// a bound that never reached the query answers a different question.
	got := callWork(t, reg, worklet.TaskActivityTool, map[string]any{
		"task": "ENG-1", "since": "last tuesday",
	})
	if !got.Failed || !strings.Contains(got.Output, "RFC3339") {
		t.Errorf("an unparseable `since` gave %q, want a refusal naming the "+
			"two shapes", got.Output)
	}
}

// A KIND task_activity DOES NOT HAVE IS REFUSED AS THE ARGUMENT, and the schema
// offers the set it is refused against.
//
// An unchecked kind (`status_changed`) reads as an empty feed. The tracker's
// read refuses it naming every kind ([worklet.ErrBadQuery]); this holds the
// tool's half: the kinds are in the schema, and the refusal says to change the
// argument, not that the tracker could not be read, which a model retries.
func TestTaskActivityOffersItsKindsAndRefusesAnotherAsTheArgument(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	entry, _ := reg.Lookup(worklet.TaskActivityTool)
	props, _ := entry.Tool.Parameters()["properties"].(map[string]any)
	kinds, _ := props["kinds"].(map[string]any)
	offered, _ := kinds["description"].(string)
	for _, kind := range worklet.ChangeKinds {
		if !strings.Contains(offered, "`"+string(kind)+"`") {
			t.Errorf("task_activity's `kinds` does not offer %s: %q", kind, offered)
		}
	}

	trk.readErr = fmt.Errorf("%w: %q is not a change kind", worklet.ErrBadQuery,
		"status_changed")
	got := callWork(t, reg, worklet.TaskActivityTool, map[string]any{
		"task": "ENG-1", "kinds": "status_changed",
	})
	if !got.Failed || !errors.Is(got.Cause, worklet.ErrBadQuery) ||
		!strings.Contains(got.Output, "change the arguments") ||
		!strings.Contains(got.Output, "status_changed") {
		t.Errorf("a kind nobody has answered %q, want a refusal of the argument", got.Output)
	}
	if got := trk.activityQuery.Kinds; len(got) != 1 || got[0] != "status_changed" {
		t.Errorf("the read was asked for %v, want the kind as it came — the "+
			"tracker's read is what refuses it", got)
	}
}

// A MISTYPED PROJECT KEY IS NOT A READ FAILURE, whose "do not conclude the
// item does not exist" keeps a model that typed ENGG for ENG believing in ENGG
// and drops the reader's sentence naming the nearest keys. A `for_type` the company does not file is
// the ARGUMENT's fault, about a project that is there.
func TestDescribingAProjectThatIsNotThereSaysSo(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		err  error
		want tools.Refusal
		says string
	}{
		{"no project", fmt.Errorf("worklet: no project \"ENGG\"; did you mean ENG: %w",
			worklet.ErrNoProject), tools.RefusalNotFound, "did you mean ENG"},
		{"no type", fmt.Errorf("worklet: no type \"epic\"; this company files bug, task: %w",
			worklet.ErrNoType), tools.RefusalInvalid, "this company files bug, task"},
	} {
		t.Run(c.name, func(t *testing.T) {
			trk := newFakeTracker()
			trk.readErr = c.err
			reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
			got := callWork(t, reg, worklet.DescribeProjectTool, map[string]any{
				"project": "ENGG",
			})
			if !got.Failed || tools.RefusalOf(got) != c.want {
				t.Fatalf("answered failed %v class %q, want %q", got.Failed,
					tools.RefusalOf(got), c.want)
			}
			// AND THE READER'S OWN ERROR IS BENEATH THE CLASS, so a caller
			// branching on the tracker's sentinel still finds it.
			if !errors.Is(got.Cause, c.err) {
				t.Errorf("the cause %v does not carry the reader's error", got.Cause)
			}
			if !strings.Contains(got.Output, c.says) || strings.Contains(got.Output, "could not read") {
				t.Errorf("the refusal %q is not the reader's own sentence", got.Output)
			}
		})
	}
}
