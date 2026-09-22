package tracker_test

import (
	"slices"
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
	listing, err := r.reader.Projects(r.t.Context(), q)
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
	detail, err := r.reader.Project(r.t.Context(), q)
	if err != nil {
		r.t.Fatalf("Project(%+v): %v", q, err)
	}
	return detail
}

// filedInto files one plain task into a NAMED project.
//
// `filedTask` beside it files into the harness's own ENG, which is all a case
// about one project needs. An ordering case needs the count columns to
// disagree with the key order, and those columns only move when a task
// actually lands in the project.
func filedInto(t *testing.T, r *roundTrip, project, id string) {
	t.Helper()
	task := newTask(id)
	task.Project = project
	task.Key = project + "-" + id
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s into %s: %v", id, project, err)
	}
	r.drain()
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
	filedTask(t, r, "open-1")
	filedTask(t, r, "open-2")
	filedTask(t, r, "shipped")

	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-ship", "shipped", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("finish a task: %v", err)
	}
	r.drain()

	listing := r.projects(tracker.ProjectQuery{Archived: tracker.ArchivedExclude})
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

// THE ARCHIVAL MODE SELECTS A SET, it does not widen one.
//
// Each of the three answers a DIFFERENT set and each carries the total of the
// set it answered. The `bool` this replaced could only exclude or include, so
// "what did we retire" was a screen's own narrowing over the wider answer —
// and a narrowing applied to a PAGE of that answer finds no archived row at
// all once the active projects fill it, which is a company with dozens of
// retired projects being told it has none.
func TestAProjectListingSelectsOneArchivalSet(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations",
		Purpose: "keep the lights on", Unit: "platform"})
	seedProject(t, r, tracker.Project{Key: "OLD", Name: "Retired",
		Archived: true})

	for _, c := range []struct {
		mode tracker.ArchivedMode
		want []string
	}{
		{tracker.ArchivedExclude, []string{"ENG", "OPS"}},
		{tracker.ArchivedOnly, []string{"OLD"}},
		{tracker.ArchivedInclude, []string{"ENG", "OLD", "OPS"}},
	} {
		listing := r.projects(tracker.ProjectQuery{Archived: c.mode})
		if got := keys(listing); !slices.Equal(got, c.want) {
			t.Errorf("archived=%s answers %v, want %v — each mode names one "+
				"set and the read selects exactly it", c.mode, got, c.want)
		}
		// AND `total` IS THE ASKED SET'S OWN COUNT. A total counted over
		// a wider set than the rows is the number a screen prints beside
		// them, so "1 of 3 projects" about an archived half is a
		// sentence comparing two different questions.
		if listing.Total != len(c.want) {
			t.Errorf("archived=%s answers total=%d, want %d — the total counts "+
				"the ASKED set", c.mode, listing.Total, len(c.want))
		}
	}
}

// THE ANSWER CARRIES THE CENSUS OF BOTH SETS, whichever one it selected.
//
// Selecting one set is what makes the listing honest, and it is also what
// leaves an empty answer ambiguous: a segmented screen asking for the live
// projects and getting none cannot tell a company with no projects from one
// that has archived every one of them, and those are opposite things to tell a
// reader. The census is the listing's own question minus its archival term, so
// the screen never has to guess and never has to ask twice.
func TestAProjectListingCarriesTheCensusOfBothSets(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations"})
	seedProject(t, r, tracker.Project{Key: "OLD", Name: "Retired", Archived: true})
	seedProject(t, r, tracker.Project{Key: "GON", Name: "Wound down", Archived: true})

	// THE SAME CENSUS UNDER EVERY MODE, because it describes the company
	// rather than the answer: a screen on the Archived segment needs the
	// active count to know the company is not empty.
	for _, mode := range tracker.ArchivedModes {
		listing := r.projects(tracker.ProjectQuery{Archived: mode})
		if listing.Census.Active != 2 || listing.Census.Archived != 2 {
			t.Errorf("archived=%s answers census %+v, want 2 active and 2 "+
				"archived — the census is the same company whichever set was "+
				"selected", mode, listing.Census)
		}
		// AND `total` IS THE CENSUS'S OWN ARITHMETIC. Counted separately
		// it is a number that can disagree with the two beside it.
		if want := listing.Census.Count(mode); listing.Total != want {
			t.Errorf("archived=%s answers total=%d and a census counting %d",
				mode, listing.Total, want)
		}
	}
}

// AND IT IS NARROWED BY THE SAME `q` AND `unit` THE LISTING IS.
//
// The census is the listing's question MINUS the archival term, not a count of
// the whole company: a directory narrowed to one unit that reported the
// company's archived total would offer a reader a segment that is empty under
// the filter they are looking through.
func TestTheProjectCensusIsNarrowedWithTheListing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations",
		Purpose: "keep the lights on", Unit: "platform"})
	seedProject(t, r, tracker.Project{Key: "OLD", Name: "Retired",
		Unit: "platform", Archived: true})
	// OUTSIDE THE UNIT, and archived — so a census that ignored `unit`
	// would report two archived where the filtered question has one.
	seedProject(t, r, tracker.Project{Key: "GON", Name: "Gone",
		Unit: "dissolved", Archived: true})
	// THE CHART THE TWO SPELLING CASES BELOW RESOLVE THROUGH: one team
	// whose id is `plat` and whose name is what these rows were filed
	// under, which is the pair a company gets the day it adds an id.
	platform := chart{{Key: "plat", Name: "platform"}}

	for _, c := range []struct {
		name         string
		q            tracker.ProjectQuery
		active, arch int
	}{
		{"unit", tracker.ProjectQuery{Unit: "platform"}, 1, 1},
		{"unit with nothing archived", tracker.ProjectQuery{Unit: "dissolved"}, 0, 1},
		{"q on the purpose", tracker.ProjectQuery{Q: "lights"}, 1, 0},
		{"q matching nothing", tracker.ProjectQuery{Q: "nothing at all"}, 0, 0},
		// AND BY EITHER SPELLING OF THE UNIT, because the rows are: a
		// team's id and its name are one narrowing, so a census cut by
		// one spelling beside rows cut by both would count a segment the
		// listing does not draw.
		{"the unit's id", tracker.ProjectQuery{Unit: "plat", Units: platform}, 1, 1},
		{"the unit's name", tracker.ProjectQuery{Unit: "platform", Units: platform}, 1, 1},
		{"the unit's name folded", tracker.ProjectQuery{Unit: "PLATFORM", Units: platform}, 1, 1},
		// A TEAM THE CHART NEVER HAD still narrows to nothing rather than
		// widening to the company.
		{"a unit nobody has", tracker.ProjectQuery{Unit: "Legal", Units: platform}, 0, 0},
	} {
		q := c.q
		q.Archived = tracker.ArchivedExclude
		listing := r.projects(q)
		if listing.Census.Active != c.active || listing.Census.Archived != c.arch {
			t.Errorf("%s: census is %+v, want %d active and %d archived — the "+
				"census is narrowed with the listing", c.name, listing.Census,
				c.active, c.arch)
		}
	}
}

// AND A COMPANY WITH NOTHING FILED CENSUSES AT ZERO, which is the one reading
// a screen acts on by going and looking at its configuration.
func TestAProjectCensusIsZeroOnACompanyWithNoProjects(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// The harness's own ENG is the only project, so narrowing past it is
	// how this suite reaches the empty company without a second harness.
	listing := r.projects(tracker.ProjectQuery{
		Archived: tracker.ArchivedExclude, Q: "no such project",
	})
	if listing.Census.Active != 0 || listing.Census.Archived != 0 ||
		listing.Census.Total() != 0 {
		t.Fatalf("census is %+v, want every count zero", listing.Census)
	}
}

// A LISTING FILTERS ON WHAT A FILTER BOX IS TYPED INTO.
func TestAProjectListingFilters(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations",
		Purpose: "keep the lights on", Unit: "platform"})
	seedProject(t, r, tracker.Project{Key: "OLD", Name: "Retired",
		Archived: true})

	live := keys(r.projects(tracker.ProjectQuery{Archived: tracker.ArchivedExclude}))
	if len(live) != 2 || live[0] != "ENG" || live[1] != "OPS" {
		t.Fatalf("the default listing is %v, want [ENG OPS] — ordered by key, "+
			"with the archived project excluded", live)
	}
	// THE PURPOSE IS SEARCHED TOO, because a filter box is typed into by
	// somebody who remembers one of the three columns.
	if got := keys(r.projects(tracker.ProjectQuery{Archived: tracker.ArchivedExclude, Q: "lights"})); len(got) != 1 ||
		got[0] != "OPS" {
		t.Fatalf("q=lights answers %v, want [OPS] — the purpose is searched", got)
	}
	if got := keys(r.projects(tracker.ProjectQuery{Archived: tracker.ArchivedExclude, Q: "operations"})); len(got) != 1 ||
		got[0] != "OPS" {
		t.Fatalf("q=operations answers %v, want [OPS] — the match is "+
			"case-insensitive", got)
	}
	if got := keys(r.projects(tracker.ProjectQuery{Archived: tracker.ArchivedExclude, Unit: "platform"})); len(got) != 1 ||
		got[0] != "OPS" {
		t.Fatalf("unit=platform answers %v, want [OPS]", got)
	}
	// A WILDCARD SOMEBODY TYPED IS A LITERAL. Unescaped, `%` matches
	// everything and a filter box silently stops filtering.
	if got := keys(r.projects(tracker.ProjectQuery{Archived: tracker.ArchivedExclude, Q: "%"})); len(got) != 0 {
		t.Fatalf("q=%%%% answers %v, want nothing — a caller's own wildcard is "+
			"escaped", got)
	}
}

// EVERY ORDERING ORDERS THE WHOLE ASKED SET, and the key breaks every tie.
//
// The ordering is the ENGINE's because the listing is a PAGE: an ordering
// applied after the cap orders the rows that survived the key order, so
// `-open` answered "the most open work among the projects whose keys sort
// first". Seeding past the cap is unnecessary to prove that — what the cap
// takes is a PREFIX of this ordering, so an ordering that is right over the
// whole set is right over the page, and an ordering that is not is wrong here
// too.
func TestAProjectListingOrdersTheWholeSet(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// ENG is the harness's own and carries no unit and no work.
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "operations",
		Unit: "zebra"})
	seedProject(t, r, tracker.Project{Key: "AAA", Name: "Zulu", Unit: "alpha"})
	// TWO OPEN ON OPS AND ONE ON AAA, so the count columns and the key
	// order disagree — an ordering that quietly fell back to the key would
	// otherwise pass every case here.
	filedInto(t, r, "OPS", "ops-1")
	filedInto(t, r, "OPS", "ops-2")
	filedInto(t, r, "AAA", "aaa-1")

	for _, c := range []struct {
		name       string
		sort       tracker.ProjectSort
		descending bool
		want       []string
	}{
		{"key", tracker.ProjectSortKey, false, []string{"AAA", "ENG", "OPS"}},
		{"-key", tracker.ProjectSortKey, true, []string{"OPS", "ENG", "AAA"}},
		// LOWERED, so `Zulu` does not sort before `operations` the way
		// this store's BINARY collation would have it.
		{"name", tracker.ProjectSortName, false, []string{"ENG", "OPS", "AAA"}},
		{"-open", tracker.ProjectSortOpen, true, []string{"OPS", "AAA", "ENG"}},
		{"open", tracker.ProjectSortOpen, false, []string{"ENG", "AAA", "OPS"}},
		// ENG NAMES NO UNIT, and an empty string is a value here: it
		// sorts first ascending rather than being dropped.
		{"unit", tracker.ProjectSortUnit, false, []string{"ENG", "AAA", "OPS"}},
	} {
		got := keys(r.projects(tracker.ProjectQuery{
			Archived: tracker.ArchivedExclude,
			Sort:     c.sort, Descending: c.descending,
		}))
		if !slices.Equal(got, c.want) {
			t.Errorf("sort=%s answers %v, want %v", c.name, got, c.want)
		}
	}

	// THE TIEBREAK IS THE KEY. `done` is zero on all three, so without it
	// the planner's walk decides and the directory reshuffles its equal
	// rows between two identical polls.
	for _, descending := range []bool{false, true} {
		got := keys(r.projects(tracker.ProjectQuery{
			Archived: tracker.ArchivedExclude,
			Sort:     tracker.ProjectSortDone, Descending: descending,
		}))
		if want := []string{"AAA", "ENG", "OPS"}; !slices.Equal(got, want) {
			t.Errorf("sort with descending=%v over an all-zero column answers "+
				"%v, want %v — the key breaks every tie, in both directions",
				descending, got, want)
		}
	}

	// AN ABSENT ORDERING IS THE KEY, which is the order this listing has
	// always had.
	got := keys(r.projects(tracker.ProjectQuery{Archived: tracker.ArchivedExclude}))
	if want := []string{"AAA", "ENG", "OPS"}; !slices.Equal(got, want) {
		t.Fatalf("an unsorted listing answers %v, want %v", got, want)
	}
}

// AN ABSENT INSTANT SORTS LAST IN BOTH DIRECTIONS.
//
// SQLite orders NULL first ascending and last descending, so a newest-first
// directory would open with every project nothing has ever been filed into.
// "Nothing recorded" is not the smallest value — it is not a value — and it is
// the rule the grid drawing this column already states.
func TestAProjectListingSortsAnAbsentLastChangeLast(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations"})
	// ENG and OPS are touched; AAA never is, so it carries no instant.
	seedProject(t, r, tracker.Project{Key: "AAA", Name: "Anything"})
	filedTask(t, r, "eng-1")
	filedInto(t, r, "OPS", "ops-1")

	for _, descending := range []bool{false, true} {
		got := keys(r.projects(tracker.ProjectQuery{
			Archived: tracker.ArchivedExclude,
			Sort:     tracker.ProjectSortLastChange, Descending: descending,
		}))
		if len(got) != 3 || got[2] != "AAA" {
			t.Errorf("sort by last_change descending=%v answers %v, want the "+
				"project with no instant last", descending, got)
		}
	}
}

// THE READ REFUSES A QUERY THAT NAMES NO ARCHIVAL SET, by name.
//
// The membership of the answer turns on it and this struct is built as a
// literal at every call site, so a default applied inside the read would be
// one no caller chose and none could see — which is exactly what the `bool`
// this replaced did.
func TestAProjectReadRefusesAnUnsetArchivalSet(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	_, err := r.reader.Projects(t.Context(), tracker.ProjectQuery{
		Level: statelog.ReadStale,
	})
	if err == nil {
		t.Fatal("a project read naming no archival set was answered — the " +
			"zero value has to be refused or it is a silent default")
	}
	if !strings.Contains(err.Error(), "archived") ||
		!strings.Contains(err.Error(), string(tracker.ArchivedOnly)) {
		t.Errorf("the refusal is %q, want the parameter and the accepted "+
			"values named", err)
	}
	// AND SO IS A SORT KEY THAT IS NOT ONE, rather than silently falling
	// back to the key order and answering a different question.
	_, err = r.reader.Projects(t.Context(), tracker.ProjectQuery{
		Level: statelog.ReadStale, Archived: tracker.ArchivedExclude,
		Sort: tracker.ProjectSort("lead"),
	})
	if err == nil || !strings.Contains(err.Error(), "lead") {
		t.Errorf("a read sorted on %q answered %v, want a refusal naming it — "+
			"a lead is resolved against the chart and has no column to order "+
			"by", "lead", err)
	}
}

// THE `archived=` AND `sort=` GRAMMAR IS PARSED ONCE, for every surface.
func TestParseProjectQuery(t *testing.T) {
	t.Parallel()

	// AN ABSENT `archived` IS THE SURFACE'S DEFAULT and it is resolved
	// HERE, which is what lets the read refuse a query that reaches it
	// without one.
	q, err := tracker.ParseProjectQuery(tracker.MapParams{})
	if err != nil {
		t.Fatalf("an empty bag: %v", err)
	}
	if q.Archived != tracker.ArchivedExclude {
		t.Errorf("an absent archived parses to %q, want %q",
			q.Archived, tracker.ArchivedExclude)
	}
	if q.Sort != "" || q.Descending {
		t.Errorf("an absent sort parses to %q/%v, want the default order",
			q.Sort, q.Descending)
	}

	q, err = tracker.ParseProjectQuery(tracker.MapParams{
		"archived": "only", "sort": "-open", "q": " lights ", "unit": " ops ",
		"limit": 7,
	})
	if err != nil {
		t.Fatalf("a full bag: %v", err)
	}
	if q.Archived != tracker.ArchivedOnly {
		t.Errorf("archived=only parses to %q", q.Archived)
	}
	if q.Sort != tracker.ProjectSortOpen || !q.Descending {
		t.Errorf("sort=-open parses to %q/%v, want open descending — the `-` "+
			"grammar is the one every grid in the dashboard writes",
			q.Sort, q.Descending)
	}
	if q.Q != "lights" || q.Unit != "ops" || q.Limit != 7 {
		t.Errorf("the rest parses to %+v", q)
	}

	// A VALUE THAT IS NOT ONE IS REFUSED BY NAME, never read as the
	// default: a filter silently ignored is a screen showing a set nobody
	// asked for.
	for _, c := range []struct{ key, value string }{
		{"archived", "yes"},
		{"archived", "active"},
		{"sort", "lead"},
		{"sort", "-progress"},
		{"sort", "-"},
	} {
		_, err := tracker.ParseProjectQuery(tracker.MapParams{c.key: c.value})
		if err == nil {
			t.Errorf("%s=%s was accepted, want a refusal", c.key, c.value)
			continue
		}
		if !strings.Contains(err.Error(), c.value) {
			t.Errorf("%s=%s is refused with %q, want the value quoted back",
				c.key, c.value, err)
		}
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

	rows := byKey(r.projects(tracker.ProjectQuery{
		Archived: tracker.ArchivedExclude,
		Units: chart{{
			Key: "platform", Name: "Platform",
			Lead: tracker.LeadRef{Handle: "ada", Kind: tracker.AuthorAgent},
		}},
	}))
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
	none := byKey(r.projects(tracker.ProjectQuery{Archived: tracker.ArchivedExclude}))
	if got := none["OPS"].Unit; got.Resolved || got.Key != "platform" {
		t.Fatalf("with no chart OPS's unit is %+v, want the raw name unresolved",
			got)
	}
}

// DESCRIBING A PROJECT IS WHAT A SEAT LEARNS ITS VOCABULARY FROM.
func TestDescribingAProjectCarriesTheVocabulary(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "in")

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
	// AND THE MAINTAINED COUNTS, because a lead reading this is asking how
	// much is in the project as well as what its vocabulary is.
	if detail.Counts.Open != 1 {
		t.Fatalf("the description counts %d open, want the one filed into it",
			detail.Counts.Open)
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
	})
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
	}); err == nil {
		t.Fatal("an unknown for_type answered — the refusal lists the types")
	}
}

// chart is a test Units: the org this reader deliberately does not hold.
//
// IT ANSWERS EITHER SPELLING, folded, because that is the seam's contract —
// a fake matching only the key would certify readers against a chart the
// engine does not have.
type chart []tracker.ChartUnit

func (c chart) ResolveUnit(ref string) (tracker.ChartUnit, bool) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	for _, unit := range c {
		if strings.ToLower(unit.Key) == ref || strings.ToLower(unit.Name) == ref {
			return unit, true
		}
	}
	return tracker.ChartUnit{}, false
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
