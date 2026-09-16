package tracker_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func (r *roundTrip) projects(q tracker.ProjectQuery) tracker.ProjectListing {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	listing, err := r.reader.Projects(r.t.Context(), q, wednesday)
	if err != nil {
		r.t.Fatalf("Projects(%+v): %v", q, err)
	}
	return listing
}

func (r *roundTrip) project(q tracker.ProjectDetailQuery) tracker.ProjectDetail {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	detail, err := r.reader.Project(r.t.Context(), q, wednesday)
	if err != nil {
		r.t.Fatalf("Project(%+v): %v", q, err)
	}
	return detail
}

// seedProject files a second project beside the harness's own ENG.
func seedProject(t *testing.T, r *roundTrip, p tracker.Project) {
	t.Helper()
	p.V = 1
	p.CreatedAt, p.UpdatedAt = wednesday, wednesday
	if _, err := r.writer.WriteDocument(t.Context(), "op-project-"+p.Key,
		tracker.ProjectSubject(p.Key), "", p, tracker.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("seed project %s: %v", p.Key, err)
	}
	r.drain()
}

// THE PROJECT COUNTS ARE READ, NOT AGGREGATED — and until this reader existed
// nothing read them at all.
//
// `tracker_projects.open_count/done_count/closed_count` are maintained by the
// task apply on every status-group change and on every arrival and departure,
// precisely so a sixty-second dashboard poll is three column reads rather than
// an aggregate over every task in the company. They were maintained and read
// by nothing: the whole point of the column was unrealised.
func TestAProjectListingReadsTheMaintainedCounts(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "open-1", nil)
	inSprint(t, r, "open-2", nil)
	inSprint(t, r, "shipped", nil)

	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-ship", "shipped", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("finish a task: %v", err)
	}
	r.drain()

	listing := r.projects(tracker.ProjectQuery{})
	if len(listing.Projects) != 1 {
		t.Fatalf("the listing has %d projects, want the one this company has",
			len(listing.Projects))
	}
	got := listing.Projects[0]
	if got.Key != "ENG" || got.Name != "Engineering" {
		t.Fatalf("the row is %s/%q, want ENG/Engineering", got.Key, got.Name)
	}
	if got.Counts.Open != 2 || got.Counts.Done != 1 {
		t.Fatalf("task_counts is %+v, want 2 open and 1 done — these are the "+
			"MAINTAINED columns, and a reader that aggregated instead would "+
			"cost O(all tasks) on every poll", got.Counts)
	}
	if listing.Total != 1 || listing.Truncated {
		t.Fatalf("total=%d truncated=%v, want 1 and false",
			listing.Total, listing.Truncated)
	}
	// AND THE ANSWER CARRIES BOTH VERDICTS. Coverage and freshness are
	// two facts and the answer never folds one into the other.
	if listing.Level != statelog.ReadStale || !listing.Complete {
		t.Fatalf("read_level=%q complete=%v, want stale and complete",
			listing.Level, listing.Complete)
	}
}

// AN ARCHIVED PROJECT IS OUT OF THE DEFAULT LISTING, and a filter reaches it.
func TestAProjectListingFiltersAndOrders(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations",
		Purpose: "keep the lights on", Unit: "platform"})
	seedProject(t, r, tracker.Project{Key: "OLD", Name: "Retired",
		Archived: true})

	live := keys(r.projects(tracker.ProjectQuery{}))
	if len(live) != 2 || live[0] != "ENG" || live[1] != "OPS" {
		t.Fatalf("the default listing is %v, want [ENG OPS] — ordered by key, "+
			"with the archived project excluded", live)
	}
	if withArchived := keys(r.projects(tracker.ProjectQuery{Archived: true})); len(withArchived) != 3 {
		t.Fatalf("archived=true answers %v, want all three", withArchived)
	}
	// THE PURPOSE IS SEARCHED TOO, because a filter box is typed into by
	// somebody who remembers one of the three columns.
	if got := keys(r.projects(tracker.ProjectQuery{Q: "lights"})); len(got) != 1 ||
		got[0] != "OPS" {
		t.Fatalf("q=lights answers %v, want [OPS] — the purpose is searched", got)
	}
	if got := keys(r.projects(tracker.ProjectQuery{Q: "operations"})); len(got) != 1 ||
		got[0] != "OPS" {
		t.Fatalf("q=operations answers %v, want [OPS] — the match is "+
			"case-insensitive", got)
	}
	if got := keys(r.projects(tracker.ProjectQuery{Unit: "platform"})); len(got) != 1 ||
		got[0] != "OPS" {
		t.Fatalf("unit=platform answers %v, want [OPS]", got)
	}
	// A WILDCARD SOMEBODY TYPED IS A LITERAL. Unescaped, `%` matches
	// everything and a filter box silently stops filtering.
	if got := keys(r.projects(tracker.ProjectQuery{Q: "%"})); len(got) != 0 {
		t.Fatalf("q=%%%% answers %v, want nothing — a caller's own wildcard is "+
			"escaped", got)
	}
}

// A PROJECT THAT NAMES A UNIT THE CHART NO LONGER HAS IS REPORTED UNRESOLVED,
// which is the finding, rather than rendered as a project naming none.
func TestAProjectsUnitIsResolvedAgainstTheChart(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations",
		Unit: "platform"})
	seedProject(t, r, tracker.Project{Key: "GON", Name: "Gone", Unit: "dissolved"})

	rows := byKey(r.projects(tracker.ProjectQuery{Units: chart{
		"platform": {"Platform", tracker.LeadRef{Handle: "ada", Kind: tracker.AuthorAgent}},
	}}))
	if got := rows["OPS"].Unit; !got.Resolved || got.Name != "Platform" {
		t.Fatalf("OPS's unit is %+v, want the chart's own name and resolved", got)
	}
	if got := rows["OPS"].Lead; got.Handle != "ada" {
		t.Fatalf("OPS's lead is %+v, want the chart's effective lead", got)
	}
	if got := rows["GON"].Unit; got.Resolved || got.Key != "dissolved" {
		t.Fatalf("GON's unit is %+v, want the raw name and resolved=false — "+
			"a project orphaned from the chart is the finding, not an absence",
			got)
	}
	// AND NAMING NO UNIT IS RESOLVED, because it is a valid state:
	// conflating it with an orphan would report every unfiled project as
	// a finding.
	if got := rows["ENG"].Unit; !got.Resolved || got.Key != "" {
		t.Fatalf("ENG's unit is %+v, want empty and resolved", got)
	}
	// AND WITH NO CHART AT ALL every row is honestly unresolved rather
	// than silently blank.
	none := byKey(r.projects(tracker.ProjectQuery{}))
	if got := none["OPS"].Unit; got.Resolved || got.Key != "platform" {
		t.Fatalf("with no chart OPS's unit is %+v, want the raw name unresolved",
			got)
	}
}

// DESCRIBING A PROJECT IS WHAT A SEAT LEARNS ITS VOCABULARY FROM.
func TestDescribingAProjectCarriesTheVocabularyAndTheSprint(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedNamedSprint(t, r, 4, "Kickoff", tracker.SprintActive)
	four := 4
	inSprint(t, r, "in", &four)

	detail := r.project(tracker.ProjectDetailQuery{Project: "eng"})
	if detail.Key != "ENG" {
		t.Fatalf("describing %q answered %q — a key is normalised, because "+
			"`eng` is the same project as `ENG` and an exact compare would "+
			"answer not-found", "eng", detail.Key)
	}
	if len(detail.Statuses) != len(tracker.Statuses) {
		t.Fatalf("the description carries %d statuses, want all %d — a seat "+
			"chooses one BY its description", len(detail.Statuses),
			len(tracker.Statuses))
	}
	for _, s := range detail.Statuses {
		if s.Label == "" || s.Description == "" || s.Group == "" {
			t.Fatalf("status %q carries %+v — a status a model chooses by "+
				"needs its label, its group and its description", s.Status, s)
		}
	}
	if len(detail.Types) == 0 {
		t.Fatalf("the description carries no types — a model told which " +
			"statuses exist and not which types would file work that is " +
			"refused on the next breath")
	}
	if detail.Sprints == nil || detail.Sprints.Active == nil {
		t.Fatalf("the description carries no active sprint, want sprint 4")
	}
	if detail.Sprints.Active.Number != 4 || detail.Sprints.Active.Name != "Kickoff" {
		t.Fatalf("the active sprint is %+v, want 4/Kickoff", detail.Sprints.Active)
	}
	// AND THE ACTIVE SPRINT CARRIES ITS FIGURES, because a lead reading
	// this is asking how the sprint is going, not only which one it is.
	if detail.Sprints.Active.Figures.Tasks != 1 {
		t.Fatalf("the active sprint holds %d tasks, want the one filed into it",
			detail.Sprints.Active.Figures.Tasks)
	}
	if len(detail.Recent) != 1 || detail.Recent[0].Number != 4 {
		t.Fatalf("recent_sprints is %+v, want the one sprint", detail.Recent)
	}
}

// AN UNKNOWN PROJECT IS REFUSED NAMING THE NEAREST, never answered empty.
//
// A reader that answered an empty description would tell a model the project
// is empty when what actually happened is that it typed the key wrong.
func TestDescribingAnUnknownProjectNamesTheNearest(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// FOUR DECOYS THAT SORT ABOVE ENG, so a refusal that merely listed the
	// first three keys alphabetically would name none of them — the
	// nearest has to be nearest to what was TYPED.
	for _, key := range []string{"AAA", "BBB", "CCC", "OPS"} {
		seedProject(t, r, tracker.Project{Key: key, Name: key})
	}

	_, err := r.reader.Project(t.Context(), tracker.ProjectDetailQuery{
		Project: "ENH", Level: statelog.ReadStale,
	}, wednesday)
	if err == nil {
		t.Fatal("describing an unknown project answered — a model that typed " +
			"a key wrong must learn the right one from the refusal")
	}
	if !strings.Contains(err.Error(), "ENG") {
		t.Fatalf("the refusal is %q, want it to name ENG — the nearest key by "+
			"shared first letter", err)
	}
	if !strings.Contains(err.Error(), "no such project") {
		t.Fatalf("the refusal is %q, want the sentinel's own words", err)
	}
}

// A PROJECT DECLARATION SHADOWS THE WORKSPACE ONE, and the description names
// which — the state a field in the middle of a move between scopes is in.
func TestADescriptionGroupsFieldsAndNamesTheShadowed(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteFields(t.Context(), "op-fields", []tracker.FieldDef{
		{ID: "f-sev", Slug: "severity", Name: "Severity",
			Type: tracker.FieldDropdown, AppliesTo: []string{"bug"}, Required: true},
		{ID: "f-note", Slug: "note", Name: "Note", Type: tracker.FieldText},
		{ID: "f-old", Slug: "old", Name: "Old", Type: tracker.FieldText,
			Archived: true},
	}); err != nil {
		t.Fatalf("WriteFields: %v", err)
	}
	r.drain()
	seedProject(t, r, tracker.Project{Key: "ENG", Name: "Engineering",
		Fields: []tracker.FieldDef{
			{ID: "f-note", Slug: "note", Name: "Project note",
				Type: tracker.FieldText, Required: true},
		}})

	detail := r.project(tracker.ProjectDetailQuery{Project: "ENG"})
	groups := map[string][]tracker.FieldDef{}
	for _, g := range detail.Fields {
		groups[g.AppliesTo] = g.Fields
	}
	if len(groups["bug"]) != 1 || groups["bug"][0].Slug != "severity" {
		t.Fatalf("the bug group is %+v, want the severity field", groups["bug"])
	}
	if len(groups[""]) != 1 || groups[""][0].Name != "Project note" {
		t.Fatalf("the every-type group is %+v, want the PROJECT's own "+
			"declaration — the nearer scope is the one in force", groups[""])
	}
	if len(detail.Shadowed) != 1 || detail.Shadowed[0] != "f-note" {
		t.Fatalf("shadowed is %v, want [f-note] — a reader has to be able to "+
			"see which definition is in force", detail.Shadowed)
	}
	for _, g := range detail.Fields {
		for _, f := range g.Fields {
			if f.Archived {
				t.Fatalf("the description offers archived field %q — what this "+
					"answer is for is what a caller may FILE", f.Slug)
			}
		}
	}

	// AND `for_type` NARROWS TO ONE TYPE while keeping the fields that
	// apply to every one — filtering those would hide exactly the fields
	// a caller filing that type must carry.
	narrowed := r.project(tracker.ProjectDetailQuery{Project: "ENG", ForType: "bug"})
	seen := map[string]bool{}
	for _, g := range narrowed.Fields {
		for _, f := range g.Fields {
			seen[f.Slug] = true
		}
	}
	if !seen["severity"] || !seen["note"] {
		t.Fatalf("for_type=bug carries %v, want both the bug field and the "+
			"one that applies to every type", seen)
	}
	if _, err := r.reader.Project(t.Context(), tracker.ProjectDetailQuery{
		Project: "ENG", ForType: "nonesuch", Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Fatal("an unknown for_type answered — the refusal lists the types")
	}
}

// chart is a test Units: the org this reader deliberately does not hold.
type chart map[string]struct {
	name string
	lead tracker.LeadRef
}

func (c chart) ResolveUnit(name string) (string, tracker.LeadRef, bool) {
	unit, found := c[name]
	return unit.name, unit.lead, found
}

func keys(l tracker.ProjectListing) []string {
	out := make([]string, 0, len(l.Projects))
	for _, p := range l.Projects {
		out = append(out, p.Key)
	}
	return out
}

func byKey(l tracker.ProjectListing) map[string]tracker.ProjectRow {
	out := make(map[string]tracker.ProjectRow, len(l.Projects))
	for _, p := range l.Projects {
		out[p.Key] = p
	}
	return out
}
