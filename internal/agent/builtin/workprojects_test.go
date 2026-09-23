package builtin_test

import (
	"encoding/json"
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

// `since` TAKES THE NEWEST POSITION A CALLER HAS SEEN, and the answer hands it
// one.
//
// The feed is newest first and `since` is a lower bound, so the value it needs
// is the position of the TOP of a page. `next_cursor` is the bottom — the
// resume point for paging older — and a caller told to pass it as `since` got
// the same page back, shorter, and read it as the end of the history. A first
// page therefore carries `newest_position`, which round-trips into `since`;
// a later page does not, because its top is older than what was already read.
func TestTaskActivityHandsBackThePositionSinceTakes(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.activity = tracker.ActivityAnswer{
		Records: []tracker.ActivityRecord{
			{ID: "newest", LogStream: "CREWLET_TRACKER_LOG", LogGeneration: 2, LogSeq: 11},
			{ID: "older", LogStream: "CREWLET_TRACKER_LOG", LogGeneration: 2, LogSeq: 9},
		},
		NextCursor: "CREWLET_TRACKER_LOG@2:9",
	}
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	first := callWork(t, reg, tracker.TaskActivityTool, map[string]any{"task": "ENG-1"})
	var answer struct {
		NewestPosition string `json:"newest_position"`
		NextCursor     string `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(first.Output), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, first.Output)
	}
	if answer.NewestPosition != "CREWLET_TRACKER_LOG@2:11" {
		t.Fatalf("newest_position = %q, want the first row's position — the "+
			"top of a newest-first page", answer.NewestPosition)
	}
	if answer.NextCursor == "" {
		t.Errorf("the page's own cursor was dropped: %s", first.Output)
	}

	// IT ROUND-TRIPS: what the answer offered is what `since` parses.
	callWork(t, reg, tracker.TaskActivityTool, map[string]any{
		"task": "ENG-1", "since": answer.NewestPosition,
	})
	if got := trk.activityQuery.Since.String(); got != answer.NewestPosition {
		t.Errorf("since = %q, want the newest_position the answer offered", got)
	}

	later := callWork(t, reg, tracker.TaskActivityTool, map[string]any{
		"task": "ENG-1", "cursor": answer.NextCursor,
	})
	if strings.Contains(later.Output, "newest_position") {
		t.Errorf("a later page offered a newest_position, which is older than "+
			"the one the first page already gave: %s", later.Output)
	}
}

// EVERY CUT BLOCK OF MY_WORK NAMES ARGUMENTS WHOSE ANSWER HOLDS ALL OF IT,
// and those arguments are ones list_work_items declares, takes and narrows on.
//
// A block stops at tracker.MyWorkRows and says so under `truncated`, and
// `rest` is the only route a seat has to the rows past the cut. So each rest
// has to narrow to its block's own people and dependencies, reach finished
// work where the block does, and reach ARCHIVED work on every block — none of
// them reads the archive, while the list leaves archived tasks and every task
// in an archived project out unless it is asked. And every key has to be one
// the list's schema declares, at the type it declares, or a client that
// validates arguments against the schema refuses the rest before the tool
// ever sees it.
func TestEveryCutBlockOfMyWorkNamesWhereTheRestIs(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.myWork = tracker.MyWork{Handle: "eng", Truncated: tracker.MyWorkTruncated{
		Priorities: true, Assigned: true, AskedOfMe: true, ChecklistItems: true,
		Collaborating: true, WatchingRecent: true, UnblockedRecent: true,
	}}
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	list, held := reg.Lookup(tracker.ListWorkItemsTool)
	if !held {
		t.Fatalf("no %s tool", tracker.ListWorkItemsTool)
	}
	declared, _ := list.Tool.Parameters()["properties"].(map[string]any)

	got := callWork(t, reg, tracker.MyWorkTool, nil)
	var answer struct {
		Rest map[string]map[string]any `json:"rest"`
	}
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("my_work answered %q: %v", got.Output, err)
	}
	isEng := func(v []string) bool { return len(v) == 1 && v[0] == "eng" }
	for block, narrows := range map[string]func(tracker.Query) bool{
		"priorities": func(q tracker.Query) bool { return q.Preset == tracker.PresetPriorities },
		"assigned":   func(q tracker.Query) bool { return isEng(q.Assignee) },
		"asked_of_me": func(q tracker.Query) bool {
			return q.AskedOf == "eng" && q.ShowClosed.All
		},
		"checklist_items": func(q tracker.Query) bool {
			return isEng(q.ChecklistAssignee) && q.ShowClosed.All
		},
		"collaborating": func(q tracker.Query) bool { return isEng(q.Collaborator) },
		"watching_recent": func(q tracker.Query) bool {
			return isEng(q.Watcher) && q.ShowClosed.All
		},
		"unblocked_recent": func(q tracker.Query) bool {
			return isEng(q.Assignee) && q.HasDependencies != nil && *q.HasDependencies &&
				q.Blocked != nil && !*q.Blocked
		},
	} {
		args, named := answer.Rest[block]
		if !named {
			t.Errorf("the cut %s block names no rest: %v", block, answer.Rest)
			continue
		}
		for key, value := range args {
			prop, known := declared[key].(map[string]any)
			if !known {
				t.Errorf("the %s block's rest names %q, which %s does not "+
					"declare", block, key, tracker.ListWorkItemsTool)
				continue
			}
			if want, have := prop["type"], jsonType(value); want != have {
				t.Errorf("the %s block's rest sends %q as %s, and %s declares "+
					"it %v", block, key, have, tracker.ListWorkItemsTool, want)
			}
		}
		if listed := callWork(t, reg, tracker.ListWorkItemsTool, args); listed.Failed {
			t.Errorf("the %s block's rest %v is refused by the list: %s",
				block, args, listed.Output)
			continue
		}
		if !narrows(trk.query) {
			t.Errorf("the %s block's rest %v listed %+v, which is not that block",
				block, args, trk.query)
		}
		if trk.query.Archived != tracker.ArchivedInclude {
			t.Errorf("the %s block's rest %v lists archived=%q — the block "+
				"holds archived work and the rest would never reach it",
				block, args, trk.query.Archived)
		}
	}

	// A BLOCK THAT WAS NOT CUT NAMES NOTHING, or "rest" would read as "more".
	trk.myWork.Truncated = tracker.MyWorkTruncated{Assigned: true}
	got = callWork(t, reg, tracker.MyWorkTool, nil)
	answer.Rest = nil
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("my_work answered %q: %v", got.Output, err)
	}
	if len(answer.Rest) != 1 || answer.Rest["assigned"] == nil {
		t.Errorf("rest = %v, want only the one cut block", answer.Rest)
	}
}

// jsonType is the JSON Schema type a decoded JSON value has.
func jsonType(value any) string {
	switch v := value.(type) {
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if v == float64(int64(v)) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case nil:
		return "null"
	}
	return "unknown"
}

// OPEN_ONLY FALSE LISTS FINISHED WORK. The grammar leaves done and cancelled
// work out of every answer unless it is asked for, so a false that sent
// nothing listed exactly what true did — and a seat checking whether a thing
// had already been filed and finished was told it had not been filed.
func TestOpenOnlyFalseListsFinishedWork(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	for _, tc := range []struct {
		args       map[string]any
		showClosed bool
	}{
		{map[string]any{}, false},
		{map[string]any{"open_only": true}, false},
		{map[string]any{"open_only": false}, true},
	} {
		if got := callWork(t, reg, tracker.ListWorkItemsTool, tc.args); got.Failed {
			t.Fatalf("%v failed: %s", tc.args, got.Output)
		}
		if trk.query.ShowClosed.All != tc.showClosed {
			t.Errorf("%v listed finished work = %v, want %v",
				tc.args, trk.query.ShowClosed.All, tc.showClosed)
		}
	}
}
