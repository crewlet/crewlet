package tracker_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// nimbus is the chart these cases read through: one team with an id, one
// without. A unit that declares no id is keyed by its name, which is the
// shape every company has until somebody opts in.
var nimbus = chart{
	{Key: "plat", Name: "Platform"},
	{Key: "Product", Name: "Product"},
}

// askWith runs a query with a chart behind it, which is what every real
// surface does — see [tracker.Query]'s Units field.
func (r *roundTrip) askWith(units tracker.Units, kv map[string]any) tracker.Answer {
	r.t.Helper()
	q, err := tracker.ParseQuery(tracker.MapParams(kv), wednesday, berlin)
	if err != nil {
		r.t.Fatalf("ParseQuery(%v): %v", kv, err)
	}
	q.Units = units
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	answer, err := r.reader.Tasks(r.t.Context(), q, wednesday)
	if err != nil {
		r.t.Fatalf("Tasks(%v): %v", kv, err)
	}
	return answer
}

// unitTask files one task under a stated unit, which is what a create with an
// explicit `unit` leaves behind: both halves are stamped and the filed one is
// never rewritten again.
func unitTask(t *testing.T, r *roundTrip, id, unit string) {
	t.Helper()
	task := newTask(id)
	task.Key = "ENG-" + id
	task.FiledUnit, task.RoutingUnit = unit, unit
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	r.drain()
}

// A UNIT FILTER FINDS THE WORK FILED UNDER EITHER SPELLING.
//
// `filed_unit` is a record of what was true and nothing rewrites it, so the
// day a founder gives a team an id the company holds both spellings of that
// team across its own history: everything filed before it under the name, and
// everything filed after it under the id. A filter comparing against the one
// string somebody typed answered with half the team's work and said nothing —
// which looks exactly like a team that has done half as much.
func TestAUnitFilterFindsBothSpellings(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	unitTask(t, r, "old", "Platform") // filed before the id existed
	unitTask(t, r, "new", "plat")     // filed after it
	unitTask(t, r, "other", "Product")

	for _, ref := range []string{"plat", "Platform", "PLATFORM", "platform"} {
		got := ids(r.askWith(nimbus, map[string]any{"unit": ref}))
		if len(got) != 2 {
			t.Errorf("unit=%s answers %v, want both the row filed under the "+
				"name and the row filed under the id", ref, got)
		}
	}
	// THE ROUTING HALF TAKES THE SAME SET, because it holds the same two
	// spellings for the same reason — a re-route stores the key of
	// whatever the writer named.
	for _, ref := range []string{"plat", "Platform"} {
		if got := ids(r.askWith(nimbus, map[string]any{"routing_unit": ref})); len(got) != 2 {
			t.Errorf("routing_unit=%s answers %v, want both rows", ref, got)
		}
	}
	// AND THE FILTER STILL NARROWS: the set is one team's spellings, not
	// every team's.
	if got := ids(r.askWith(nimbus, map[string]any{"unit": "Product"})); len(got) != 1 ||
		got[0] != "other" {
		t.Errorf("unit=Product answers %v, want only the row filed into it", got)
	}
	// A TEAM NOBODY HAS MATCHES NOTHING rather than erroring or widening:
	// a filter for a unit the chart never had is answerable, and the
	// answer is that there is no such work.
	if got := ids(r.askWith(nimbus, map[string]any{"unit": "Legal"})); len(got) != 0 {
		t.Errorf("unit=Legal answers %v, want nothing", got)
	}
}

// WITH NO CHART THE REFERENCE IS MATCHED LITERALLY, which is the honest
// answer for a process holding none — the same state [tracker.Units] calls
// meaningful for rendering. It is not a silent widening and not a failure.
func TestAUnitFilterWithNoChartMatchesTheLiteral(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	unitTask(t, r, "old", "Platform")
	unitTask(t, r, "new", "plat")

	if got := ids(r.askWith(nil, map[string]any{"unit": "plat"})); len(got) != 1 ||
		got[0] != "new" {
		t.Errorf("with no chart unit=plat answers %v, want the row that holds "+
			"exactly that", got)
	}
}

// A BOARD'S UNIT COLUMN IS HEADED WITH THE TEAM'S NAME.
//
// What the rows hold is the unit's KEY, which is an id on any company that
// gave its units one — a word chosen to survive a rename precisely because
// nobody reads it. Without the chart behind the axis, opting into ids
// re-headed every board in the company with a slug.
func TestAUnitColumnIsHeadedWithTheTeamsName(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	unitTask(t, r, "new", "plat")
	unitTask(t, r, "gone", "dissolved")

	answer := r.askWith(nimbus, map[string]any{
		"container": "project:ENG", "group_by": "unit",
	})
	if got := groupOf(t, answer, "plat"); got.Label != "Platform" {
		t.Errorf("the plat column is headed %q, want Platform", got.Label)
	}
	// A KEY THE CHART CANNOT RESOLVE KEEPS AN EMPTY LABEL, because a
	// client renders the label or falls back to the key — and the key is
	// what is in the rows and what a filter on that column takes. A
	// stand-in word would name a team nobody can search for.
	if got := groupOf(t, answer, "dissolved"); got.Label != "" {
		t.Errorf("a column for a unit the chart has lost is headed %q, want "+
			"the stored key to stand", got.Label)
	}
}

// A WORKLOAD NARROWS BY EITHER SPELLING TOO. The screen that asks was handed
// a unit from somewhere else — a chart, a project row, a URL — and which of
// the two names it holds is not something the reader can assume.
func TestAWorkloadNarrowsByEitherSpelling(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "ENG", Name: "ENG", Unit: "plat"})
	seedProject(t, r, tracker.Project{Key: "PROD", Name: "PROD", Unit: "Product"})
	pointedTask(t, r, "a1", 4, "ada")
	pointedTask(t, r, "a2", 9, "ada", "PROD")

	for _, ref := range []string{"plat", "Platform", "PLATFORM"} {
		ada := loadOf(t, r.workload(tracker.WorkloadQuery{Unit: ref, Units: nimbus}), "ada")
		if ada.Points != 4 {
			t.Errorf("narrowed to %s, ada holds %v points, want only the 4 "+
				"that team owns", ref, ada.Points)
		}
	}
	// AND A TEAM WITH NO WORK IS STILL EMPTY rather than the whole
	// company: resolving the reference must not drop the filter.
	if rows := r.workload(tracker.WorkloadQuery{Unit: "Legal", Units: nimbus}).Rows; len(rows) != 0 {
		t.Errorf("a unit with no work answers %d rows, want none", len(rows))
	}
}

// A PROJECT'S STORED UNIT RENDERS AS THE TEAM'S NAME, and is filtered by
// either spelling.
//
// The column is chart-owned and holds the key, so a listing that showed it
// raw would label every project with a slug — and a filter that compared
// against it raw would miss the name the screen had just displayed.
func TestAProjectsUnitKeyRendersAsItsName(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations", Unit: "plat"})

	rows := byKey(r.projects(tracker.ProjectQuery{Archived: tracker.ArchivedExclude, Units: nimbus}))
	got := rows["OPS"].Unit
	if !got.Resolved || got.Name != "Platform" || got.Key != "plat" {
		t.Fatalf("OPS's unit is %+v, want the stored key with the team's "+
			"current name beside it", got)
	}
	for _, ref := range []string{"plat", "Platform", "platform"} {
		listed := keys(r.projects(tracker.ProjectQuery{
			Archived: tracker.ArchivedExclude, Unit: ref, Units: nimbus,
		}))
		if len(listed) != 1 || listed[0] != "OPS" {
			t.Errorf("unit=%s lists %v, want [OPS]", ref, listed)
		}
	}
	if listed := keys(r.projects(tracker.ProjectQuery{
		Archived: tracker.ArchivedExclude, Unit: "Legal", Units: nimbus,
	})); len(listed) != 0 {
		t.Errorf("unit=Legal lists %v, want nothing", listed)
	}
}

// AN ITEM'S OWN PAGE READS THE TEAM'S NAME, whichever spelling its row holds.
//
// The document's two strings stay exactly as they were written — they are the
// record — and the answer carries the chart's reading of them beside it. A
// screen rendering the document raw showed `plat` where the same company's
// board column said `Platform`.
func TestATasksDetailResolvesBothUnitsAgainstTheChart(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	unitTask(t, r, "old", "Platform") // filed before the id existed
	unitTask(t, r, "new", "plat")     // filed after it
	unitTask(t, r, "gone", "dissolved")
	unitTask(t, r, "none", "")

	detail := func(id string) tracker.TaskDetail {
		t.Helper()
		got, err := r.reader.Task(t.Context(), id,
			tracker.DetailWants{Units: nimbus},
			statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return got
	}
	for _, id := range []string{"old", "new"} {
		got := detail(id)
		if got.Units == nil {
			t.Fatalf("%s carries no resolved units at all", id)
		}
		if !got.Units.Filed.Resolved || got.Units.Filed.Name != "Platform" {
			t.Errorf("%s's filed unit resolves to %+v, want Platform",
				id, got.Units.Filed)
		}
		// THE ROW'S OWN SPELLING IS WHAT THE KEY CARRIES, because that is
		// what the record holds and what a filter link has to send.
		if got.Units.Filed.Key != got.Task.FiledUnit {
			t.Errorf("%s's filed key is %q, want the stored %q",
				id, got.Units.Filed.Key, got.Task.FiledUnit)
		}
		// AND THE ROUTING HALF IS RESOLVED TOO: a screen draws both.
		if !got.Units.Routing.Resolved || got.Units.Routing.Name != "Platform" {
			t.Errorf("%s's routing unit resolves to %+v, want Platform",
				id, got.Units.Routing)
		}
		// THE DOCUMENT IS UNTOUCHED — it is the record.
		if got.Task.FiledUnit == "" || got.Task.RoutingUnit == "" {
			t.Errorf("%s's document lost its own unit strings: %+v", id, got.Task)
		}
	}
	// A TEAM THE CHART HAS LOST IS FLAGGED rather than rendered as a team
	// named nothing: `resolved: false` is the finding.
	if got := detail("gone"); got.Units == nil || got.Units.Filed.Resolved ||
		got.Units.Filed.Key != "dissolved" || got.Units.Filed.Name != "" {
		t.Errorf("a task filed against a team the chart has lost reads %+v, "+
			"want the stored key unresolved", got.Units)
	}
	// AND A TASK FILED INTO NO TEAM CARRIES NOTHING, because that is what
	// its two empty strings already say.
	if got := detail("none"); got.Units != nil {
		t.Errorf("a task filed into no unit carries %+v, want no units block", got.Units)
	}
	// WITH NO CHART every reference is honestly unresolved rather than
	// silently blank — the state a process holding no org is in.
	blind, err := r.reader.Task(t.Context(), "new", tracker.DetailWants{},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read new with no chart: %v", err)
	}
	if blind.Units == nil || blind.Units.Filed.Resolved ||
		blind.Units.Filed.Key != "plat" {
		t.Errorf("with no chart the filed unit reads %+v, want the raw key "+
			"unresolved", blind.Units)
	}
}

// A TEAM'S VIEW STRIP IS ONE STRIP, whichever of the team's two spellings it
// is asked for by.
//
// A container is an ADDRESS, and every other kind is canonical by
// construction — a project key is upper-cased at the parser, a person is a
// handle. A unit is the one with two spellings, so a view saved against
// `unit:Engineering` was invisible from `unit:eng`, and neither surface could
// tell that from a container nobody has saved a view in.
func TestAUnitsViewStripIsOneStripUnderEitherSpelling(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// SAVED UNDER THE NAME, which is what a strip written before the team
	// had an id holds — and what nothing may rewrite.
	view := aView("v-unit", func(v *tracker.View) {
		v.Container = tracker.Container{Kind: tracker.ContainerUnit, ID: "Platform"}
	})
	if _, err := r.writer.WriteView(t.Context(), "op-v-unit", view); err != nil {
		t.Fatalf("save a unit view: %v", err)
	}
	r.drain()

	for _, ref := range []string{"Platform", "plat", "PLAT", "platform"} {
		listing, err := r.reader.Views(t.Context(), tracker.ViewQuery{
			Container: tracker.CanonicalContainer(nimbus,
				tracker.Container{Kind: tracker.ContainerUnit, ID: ref}),
			Units: nimbus, Level: statelog.ReadStale,
		})
		if err != nil {
			t.Fatalf("read the strip of %q: %v", ref, err)
		}
		if !slices.Contains(stripKeys(listing), "v-unit") {
			t.Errorf("the strip of %q carries %v, want the view saved under "+
				"the team's other spelling", ref, stripKeys(listing))
		}
	}
	// AND ANOTHER TEAM'S STRIP IS STILL ANOTHER STRIP.
	other, err := r.reader.Views(t.Context(), tracker.ViewQuery{
		Container: tracker.Container{Kind: tracker.ContainerUnit, ID: "Product"},
		Units:     nimbus, Level: statelog.ReadStale,
	})
	if err != nil {
		t.Fatalf("read Product's strip: %v", err)
	}
	if slices.Contains(stripKeys(other), "v-unit") {
		t.Error("one team's saved view appeared in another team's strip")
	}
}

// AND A CONTAINER IS STORED UNDER THE TEAM'S KEY, which is what keeps the
// strips from splitting again on the next save.
func TestACanonicalContainerCarriesTheUnitsKey(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"Platform", "PLAT", "plat"} {
		got := tracker.CanonicalContainer(nimbus,
			tracker.Container{Kind: tracker.ContainerUnit, ID: ref})
		if got.ID != "plat" {
			t.Errorf("unit:%s is keyed %q, want plat", ref, got.ID)
		}
	}
	// EVERY OTHER KIND IS LEFT EXACTLY AS IT IS: a project key is already
	// upper-cased where it is parsed, and a person is a handle.
	project := tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"}
	if got := tracker.CanonicalContainer(nimbus, project); got != project {
		t.Errorf("a project container became %+v", got)
	}
	// AND A TEAM THE CHART HAS LOST KEEPS ITS OWN ADDRESS, because the
	// strip saved against it is still that strip.
	gone := tracker.Container{Kind: tracker.ContainerUnit, ID: "dissolved"}
	if got := tracker.CanonicalContainer(nimbus, gone); got != gone {
		t.Errorf("an unresolvable unit container became %+v", got)
	}
}
