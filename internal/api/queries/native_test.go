package queries_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// stubWork and stubPages record what filter the surface built, which is the
// half a round trip through a real reader would hide: the questions here are
// almost entirely about turning a query string into a Filter, and a test that
// only checked the rows would pass with every filter dropped.
type stubWork struct {
	inbox         tracker.InboxAnswer
	inboxQuery    tracker.InboxQuery
	routing       tracker.RoutingAnswer
	routingQuery  tracker.RoutingQuery
	ranked        []tracker.Ranked
	searchText    string
	searchLimit   int
	searchMode    knowledge.Mode
	searchOutcome knowledge.Outcome
	query         tracker.Query
	answer        tracker.Answer
	detail        tracker.TaskDetail
	views         tracker.ViewQuery
	listing       tracker.ViewListing
	personQuery   tracker.PersonQuery
	person        tracker.PersonState

	projectQuery  tracker.ProjectQuery
	projects      tracker.ProjectListing
	detailQuery   tracker.ProjectDetailQuery
	project       tracker.ProjectDetail
	workloadQuery tracker.WorkloadQuery
	workload      tracker.WorkloadAnswer

	// The instant and the clock each day-cutting read was handed, so a
	// test can hold a surface to the company's own midnight.
	expandNow, workloadNow, myWorkNow    time.Time
	expandZone, workloadZone, myWorkZone *time.Location

	expandViewer  tracker.Viewer
	activityQuery tracker.ActivityQuery
	activity      tracker.ActivityAnswer
	myWorkQuery   tracker.MyWorkQuery
	myWork        tracker.MyWork

	catalogueQuery tracker.CatalogueQuery
	taskLevel      statelog.ReadLevel
	taskFresh      statelog.Freshness
	taskWants      tracker.DetailWants

	err error
}

func (s *stubWork) Projects(_ context.Context, q tracker.ProjectQuery) (
	tracker.ProjectListing, error) {

	s.projectQuery = q
	return s.projects, s.err
}

func (s *stubWork) Project(_ context.Context, q tracker.ProjectDetailQuery) (
	tracker.ProjectDetail, error) {

	s.detailQuery = q
	return s.project, s.err
}

func (s *stubWork) Workload(_ context.Context, q tracker.WorkloadQuery,
	now time.Time, loc *time.Location) (tracker.WorkloadAnswer, error) {

	s.workloadQuery = q
	s.workloadNow, s.workloadZone = now, loc
	return s.workload, s.err
}

func (s *stubWork) Activity(_ context.Context, q tracker.ActivityQuery,
	_ time.Time) (tracker.ActivityAnswer, error) {

	s.activityQuery = q
	return s.activity, s.err
}

func (s *stubWork) MyWork(_ context.Context, q tracker.MyWorkQuery,
	now time.Time, loc *time.Location) (tracker.MyWork, error) {

	s.myWorkQuery = q
	s.myWorkNow, s.myWorkZone = now, loc
	return s.myWork, s.err
}

func (s *stubWork) Views(_ context.Context, q tracker.ViewQuery) (tracker.ViewListing, error) {
	s.views = q
	return s.listing, s.err
}

func (s *stubWork) ExpandedQuery(_ context.Context, params map[string]any,
	viewer tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error) {

	// RECORDED, because the viewer is the one input to this call that the
	// parsed query does not carry: it is a property of the surface rather
	// than a filter, so a handler that dropped it would still return rows.
	s.expandViewer = viewer
	s.expandNow, s.expandZone = now, loc
	return tracker.ParseQuery(tracker.MapParams(params), now, loc)
}

func (s *stubWork) Catalogue(_ context.Context,
	q tracker.CatalogueQuery) (tracker.CatalogueAnswer, error) {

	s.catalogueQuery = q
	return tracker.CatalogueAnswer{}, nil
}

func (s *stubWork) Person(_ context.Context, q tracker.PersonQuery, _ time.Time) (tracker.PersonState, error) {
	s.personQuery = q
	return s.person, s.err
}

func (s *stubWork) Inbox(_ context.Context, q tracker.InboxQuery, _ time.Time) (tracker.InboxAnswer, error) {
	s.inboxQuery = q
	return s.inbox, s.err
}

func (s *stubWork) Routing(_ context.Context, q tracker.RoutingQuery, _ time.Time) (
	tracker.RoutingAnswer, error) {

	s.routingQuery = q
	return s.routing, s.err
}

// Search is the stub's half of the SEPARATE search seam — see
// [queries.WorkSearcher]. It is on this type for the harness's convenience
// only; the surface takes the two independently, and a case that wants a node
// with a board and no index leaves `WorkSearch` nil.
func (s *stubWork) Search(_ context.Context, q tracker.SearchQuery) (tracker.SearchAnswer, error) {
	s.searchText, s.searchLimit, s.searchMode = q.Text, q.Limit, q.Mode
	return tracker.SearchAnswer{Hits: s.ranked, Outcome: s.searchOutcome}, s.err
}

func (s *stubWork) Tasks(_ context.Context, q tracker.Query, _ time.Time) (tracker.Answer, error) {
	s.query = q
	return s.answer, s.err
}

func (s *stubWork) Task(_ context.Context, _ string, want tracker.DetailWants,
	fresh statelog.Freshness) (tracker.TaskDetail, error) {

	s.taskLevel, s.taskFresh = fresh.Level, fresh
	s.taskWants = want
	return s.detail, s.err
}

type stubPages struct {
	filter pages.Filter
	list   []pages.Summary
	err    error

	// level is what the surface asked for, so a route that stopped naming
	// one is visible: an unset level is what made every page read on this
	// surface a label rather than a guarantee.
	level statelog.ReadLevel

	// fresh is the whole ask, so a route that carried the level and
	// dropped the bounds or the floor beside it is visible too.
	fresh statelog.Freshness

	// activity and revision are what the two newest reads were asked, so a
	// case can prove a filter reached the reader rather than only that
	// rows came back.
	activity     pages.PageActivityQuery
	changes      []pages.PageChange
	revision     pages.Revision
	revisionHeld bool
	askedPage    string
	askedVersion int
}

func (s *stubPages) List(_ context.Context, f pages.Filter,
	fresh statelog.Freshness,
) (pages.Listing, error) {
	s.filter, s.level, s.fresh = f, fresh.Level, fresh
	return pages.Listing{Pages: s.list, Level: fresh.Level, Complete: true}, s.err
}

func (s *stubPages) Get(_ context.Context, _ string,
	fresh statelog.Freshness,
) (pages.Detail, error) {
	s.level, s.fresh = fresh.Level, fresh
	return pages.Detail{}, s.err
}

func (s *stubPages) Containers(_ context.Context,
	fresh statelog.Freshness,
) ([]pages.ContainerListing, error) {
	s.level, s.fresh = fresh.Level, fresh
	return nil, s.err
}

func (s *stubPages) Activity(_ context.Context, q pages.PageActivityQuery) (
	pages.PageActivity, error) {

	s.activity = q
	s.level, s.fresh = q.Freshness.Level, q.Freshness
	return pages.PageActivity{
		Changes: s.changes, Level: q.Freshness.Level, Complete: true,
	}, s.err
}

func (s *stubPages) Revision(_ context.Context, pageID string, version int,
	fresh statelog.Freshness,
) (pages.Revision, bool, error) {
	s.askedPage, s.askedVersion = pageID, version
	s.level, s.fresh = fresh.Level, fresh
	return s.revision, s.revisionHeld, s.err
}

// personalQuestions are the four scoped by the caller's own seat — see
// Sources.viewerParty. They refuse an anonymous caller who names somebody
// else, so a sweep that walks every native question has to present a
// credential for these four. Named once rather than per sweep: the set grew
// from one to four, and each sweep that spelled it as `== "work_my_work"`
// silently stopped covering the rest.
var personalQuestions = map[string]bool{
	"work_my_work":  true,
	"work_person":   true,
	"work_inbox":    true,
	"conversations": true,
}

// askNative runs one question against a registry built from these sources,
// returning the error rather than failing on it: every case here is about a
// refusal, which the shared `ask` helper turns into a Fatalf.
func askNative(t *testing.T, s queries.Sources, what string, params map[string]any) (any, error) {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, s)
	return r.Answer(t.Context(), what, params, "")
}

// askAsOperator is askNative with a token, for the questions RegisterOperator
// guards. Separate rather than a parameter on the one above, so no case here
// can hand itself a credential by accident.
func askAsOperator(t *testing.T, s queries.Sources, what string,
	params map[string]any) (any, error) {

	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, s)
	return r.Answer(t.Context(), what, params, "ops-1")
}

// A QUESTION WITH NO SOURCE IS UNREGISTERED, not registered-and-empty. A
// company on Jira has no native record for this node to have a copy of, and a
// board answering "no work" would say the opposite of what is true.
func TestTheNativeQuestionsAreAbsentWithoutTheirReaders(t *testing.T) {
	for _, what := range []string{"work_items", "work_item", "pages", "page", "containers"} {
		if _, err := askNative(t, queries.Sources{}, what, nil); !errors.Is(err, queries.ErrUnknown) {
			t.Errorf("%s on a node with no native backend answered %v, want unknown", what, err)
		}
	}
}

// A COPY THAT CANNOT ANSWER SAYS SO. Flattening a failure to an empty list
// would tell a person the company has no work — an answer they act on, by
// filing the duplicate or concluding the migration failed.
//
// THE HYDRATION SENTINEL IS GONE with the projection that raised it: both
// native backends are state-log domains now and both say how far behind they
// are through the coverage on the answer itself, which is a POSITION rather
// than a boolean. What still has to hold is that a read which FAILED is not
// rendered as a read that found nothing.
func TestAReadThatFailedIsNotRenderedAsEmpty(t *testing.T) {
	sentinel := errors.New("the store could not be reached")
	w := &stubWork{err: sentinel}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_items", nil); !errors.Is(err, sentinel) {
		t.Errorf("a failed board read answered %v, want the failure", err)
	}
	p := &stubPages{err: sentinel}
	if _, err := askNative(t, queries.Sources{Pages: p}, "pages", nil); !errors.Is(err, sentinel) {
		t.Errorf("a failed page listing answered %v, want the failure", err)
	}
}

// A RECORD THAT IS NOT THERE IS NOT A FAILURE. A mistyped item key must read
// to an operator as a dead link, not as the server being broken.
func TestAMissingRecordIsNotFound(t *testing.T) {
	w := &stubWork{err: tracker.ErrNoTask}
	_, err := askNative(t, queries.Sources{Work: w}, "work_item", map[string]any{"id": "ENG-999"})
	if !errors.Is(err, queries.ErrNotFound) {
		t.Errorf("a missing item answered %v, want not-found", err)
	}
	p := &stubPages{err: pages.ErrNotFound}
	_, err = askNative(t, queries.Sources{Pages: p}, "page", map[string]any{"id": "nope"})
	if !errors.Is(err, queries.ErrNotFound) {
		t.Errorf("a missing page answered %v, want not-found", err)
	}
}

// THE ITEM QUERY ASKS FOR EVERY PART, the custom fields included.
//
// A detail read without them rendered a task filed with a severity as one that
// carried none — confidently, in a properties panel, beside a board that had
// just filtered on that very field. The four are asked for together because
// one screen draws all four and a second read for the fields would be a second
// answer that can disagree with the first.
func TestTheItemQueryAsksForEveryPart(t *testing.T) {
	w := &stubWork{}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_item",
		map[string]any{"id": "ENG-1"}); err != nil {
		t.Fatalf("work_item: %v", err)
	}
	want := tracker.DetailWants{Comments: true, History: true, Links: true, Fields: true}
	if w.taskWants != want {
		t.Errorf("work_item asked the reader for %+v, want %+v", w.taskWants, want)
	}
}

// AND IT ASKS WITH THE CHART, so the properties panel reads a team's NAME
// where the row holds its key.
//
// What a task's unit fields hold is [org.Unit.Key] — an id on any company
// that gave its units one, which is a word chosen so that a rename moves
// nothing and therefore a word nobody reads. Without the chart this answer
// carried the id alone, and an item page said `eng` beside a board column
// that said Engineering. An unresolved reference is also a FINDING — the team
// has left the chart — so a surface that holds a chart and does not pass it
// reports every task as orphaned.
func TestTheItemQueryAsksWithTheChart(t *testing.T) {
	w := &stubWork{}
	cfg := company(t)
	if _, err := askNative(t, queries.Sources{
		Work: w, Company: func() *config.Company { return cfg },
	}, "work_item", map[string]any{"id": "ENG-1"}); err != nil {
		t.Fatalf("work_item: %v", err)
	}
	if w.taskWants.Units == nil {
		t.Error("work_item read the item with no chart, so its unit fields " +
			"render as a key nobody reads and as a team the chart has lost")
	}
}

// EVERY FILTER REACHES THE READER. A filter honoured on one transport and
// dropped on the other is the exact divergence this package exists to
// prevent, and a board silently ignoring `assignee` looks like a board with
// nothing assigned.
func TestABoardFilterReachesTheReader(t *testing.T) {
	w := &stubWork{}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_items", map[string]any{
		"container": "project:eng", "assignee": "swe", "tag": "urgent",
		"status": "todo,in_progress", "q": "deploy", "limit": 10,
	}); err != nil {
		t.Fatalf("work_items: %v", err)
	}
	got := w.query
	// UPPERCASED, because a project key is compared upper everywhere else
	// and a board that only matched the case somebody typed would answer
	// empty for the same project spelled two ways.
	if got.Scope.Project != "ENG" {
		t.Errorf("project reached the reader as %q, want it upper-cased",
			got.Scope.Project)
	}
	if len(got.Assignee) != 1 || got.Assignee[0] != "swe" {
		t.Errorf("assignee reached the reader as %v", got.Assignee)
	}
	if len(got.Tags.Tags) != 1 || got.Tags.Tags[0] != "urgent" || got.Text != "deploy" {
		t.Errorf("a filter was dropped on the way: %+v", got)
	}
	if len(got.Status) != 2 || got.Status[0] != tracker.StatusTodo {
		t.Errorf("statuses reached the reader as %v", got.Status)
	}
	if got.Limit != 10 {
		t.Errorf("paging reached the reader as limit=%d", got.Limit)
	}
}

// AN ABSENT SCOPE IS NEITHER THE WORKSPACE NOR A PROJECT, and the caller
// resolves it from its own surface. Defaulting an omitted container to the
// workspace would make the cheapest thing to type the most expensive query in
// the system — every seat's idle board scanning every project.
func TestAnAbsentContainerIsNeither(t *testing.T) {
	for name, tc := range map[string]struct {
		params    map[string]any
		workspace bool
		project   string
	}{
		"absent":    {params: nil},
		"workspace": {params: map[string]any{"container": "workspace"}, workspace: true},
		"a project": {params: map[string]any{"container": "project:ENG"}, project: "ENG"},
	} {
		t.Run(name, func(t *testing.T) {
			w := &stubWork{}
			if _, err := askNative(t, queries.Sources{Work: w}, "work_items", tc.params); err != nil {
				t.Fatalf("work_items: %v", err)
			}
			if w.query.Scope.Workspace != tc.workspace || w.query.Scope.Project != tc.project {
				t.Errorf("the scope reached the reader as %+v", w.query.Scope)
			}
		})
	}
}

// The same three states for `skills`, and all three are real: only the
// tool-skill pages, everything but them, and everything.
func TestSkillsIsThreeStated(t *testing.T) {
	p := &stubPages{}
	if _, err := askNative(t, queries.Sources{Pages: p}, "pages", nil); err != nil {
		t.Fatalf("pages: %v", err)
	}
	if p.filter.Skills != nil {
		t.Errorf("an absent `skills` reached the reader as %v", *p.filter.Skills)
	}
	if _, err := askNative(t, queries.Sources{Pages: p}, "pages", map[string]any{"skills": false}); err != nil {
		t.Fatalf("pages: %v", err)
	}
	if p.filter.Skills == nil || *p.filter.Skills {
		t.Error("skills=false reached the reader as absent or true")
	}
}

// A BAD ENUM IS REFUSED NAMING THE CLOSED SET, rather than silently matching
// nothing: a board that answered empty for `status=done` (which is not a
// status here) would send somebody looking for the missing items.
func TestAnUnknownStatusIsRefusedNamingTheSet(t *testing.T) {
	_, err := askNative(t, queries.Sources{Work: &stubWork{}}, "work_items",
		map[string]any{"status": "finished"})
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("an unknown status answered %v, want bad params", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "finished") ||
		!strings.Contains(msg, "six") {
		t.Errorf("the refusal does not name the value and the set: %q", msg)
	}
}

// THE BOARD'S TOTAL IS A SEPARATE HINT, never len(items). A header reporting
// the page size as the project's size says "50 items" for every project with
// more than fifty — and an EXACT total over an unbounded set is the one query
// in this grammar that turns a poll into a scan, which is why it says hint.
func TestTheBoardCarriesItsOwnTotalAndReadLevel(t *testing.T) {
	w := &stubWork{answer: tracker.Answer{
		Rows:      []tracker.TaskRow{{ID: "i1", Key: "ENG-1"}},
		TotalHint: 42, Level: statelog.ReadStale, Complete: true,
	}}
	got, err := askNative(t, queries.Sources{Work: w}, "work_items", nil)
	if err != nil {
		t.Fatalf("work_items: %v", err)
	}
	payload, _ := got.(map[string]any)
	if payload["total_hint"] != 42 {
		t.Errorf("the board carried total_hint=%v with one row", payload["total_hint"])
	}
	// AND THE READ LEVEL RIDES WITH IT. A screen that could not say how
	// stale its answer is would render a lagging node identically to a
	// caught-up one — which is the one thing this framework's read levels
	// exist to make impossible.
	if payload["read_level"] != statelog.ReadStale {
		t.Errorf("the board carried read_level=%v", payload["read_level"])
	}
	if payload["complete"] != true {
		t.Errorf("the board carried complete=%v", payload["complete"])
	}
}

// A NODE THAT IS BEHIND ANSWERS "COME BACK", NOT "THE SERVER BROKE".
//
// This is a difference a client acts on: a 503 with a hint refreshes the
// screen in a few seconds, where a 500 tells it to give up on a screen that
// would have worked. The API reference has documented the 503 since the
// surface existed, and nothing produced it — every read refusal reached the
// caller as a plain failure once the tracker moved onto the log.
func TestAReadThisNodeCannotServeYetIsUnavailableRatherThanFailed(t *testing.T) {
	t.Parallel()
	work := &stubWork{err: &statelog.Refused{
		Code: statelog.RefuseBehind, Level: statelog.ReadSession,
		Detail: "this node is 40 000 records behind", RetryAfter: 12 * time.Second,
	}}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Work: work})

	_, err := r.Answer(t.Context(), "work_items", map[string]any{}, "")
	if !errors.Is(err, queries.ErrUnavailable) {
		t.Fatalf("a node that is behind answered %v — a read refusal that "+
			"reaches a client as a plain failure is rendered as a broken "+
			"server on a screen that would have worked in a few seconds", err)
	}
	// AND THE HINT IS THE REFUSAL'S OWN, derived from how far behind this
	// node is over how fast it is draining. A flat five seconds is wrong
	// in both directions on one fleet.
	if got := queries.RetryAfter(err); got != 12*time.Second {
		t.Errorf("the retry hint is %s, want the refusal's own 12s", got)
	}
}

// AND A REFUSAL WAITING CANNOT CLEAR IS STILL A FAILURE.
//
// A node holding a record it cannot decode will not catch up however long the
// caller waits, so a Retry-After there sends a client round a loop that cannot
// terminate. The classification is the state log's own rather than a second
// list on this side.
func TestARefusalWaitingCannotClearIsNotAnInvitationToRetry(t *testing.T) {
	t.Parallel()
	work := &stubWork{err: &statelog.Refused{
		Code:   statelog.RefuseDeferred,
		Level:  statelog.ReadSession,
		Detail: "this node holds a record at a version it cannot decode",
	}}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Work: work})

	_, err := r.Answer(t.Context(), "work_items", map[string]any{}, "")
	if err == nil {
		t.Fatal("a refused read answered successfully")
	}
	if errors.Is(err, queries.ErrUnavailable) {
		t.Fatalf("a refusal waiting cannot clear was reported as %v — a client "+
			"told to come back goes round a loop that cannot terminate",
			queries.ErrUnavailable)
	}
}

// A REFUSAL ABOUT THE REQUEST STAYS ONE, even when what it wraps is a refusal
// the registry would otherwise call transient. The caller has to change the
// request; "come back" would send the identical one round a loop.
func TestARefusalAboutTheRequestIsNeverReclassifiedAsUnavailable(t *testing.T) {
	t.Parallel()
	behind := &statelog.Refused{Code: statelog.RefuseBehind, Level: statelog.ReadSession,
		RetryAfter: time.Second}
	work := &stubWork{err: fmt.Errorf("%w: %w", tracker.ErrTooBroad, behind)}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Work: work})

	_, err := r.Answer(t.Context(), "work_items", map[string]any{}, "")
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("err = %v, want ErrBadParams", err)
	}
	if errors.Is(err, queries.ErrUnavailable) {
		t.Errorf("a refusal about the request was also reported as %v", queries.ErrUnavailable)
	}
}

// A VIEW STRIP'S CONTAINER IS THE QUERY GRAMMAR'S OWN SPELLING.
//
// A screen reaches the strip and then the tasks in it; two spellings of one
// container would make the tab it lands on belong to a different project from
// the rows beneath it.
func TestAViewStripTakesTheContainerTheBoardTakes(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		kind, id string
	}{
		{"workspace", tracker.ContainerWorkspace, ""},
		{"project:eng", tracker.ContainerProject, "ENG"},
		{"project:ENG", tracker.ContainerProject, "ENG"},
		{"unit:engineering", tracker.ContainerUnit, "engineering"},
		{"person:ana", tracker.ContainerPerson, "ana"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			w := &stubWork{}
			// AS AN OPERATOR, because naming somebody else's
			// handle is what the credential buys — see
			// TestAStripIsOnlyPersonalisedByAViewerTheCallerMayName.
			if _, err := askAsOperator(t, queries.Sources{Work: w}, "work_views",
				map[string]any{"container": tc.raw, "viewer": "ana"}); err != nil {
				t.Fatalf("work_views: %v", err)
			}
			if w.views.Container.Kind != tc.kind || w.views.Container.ID != tc.id {
				t.Fatalf("%q reached the reader as %s %q, want %s %q", tc.raw,
					w.views.Container.Kind, w.views.Container.ID, tc.kind, tc.id)
			}
			if w.views.Viewer.Handle != "ana" {
				t.Fatalf("the viewer reached the reader as %+v", w.views.Viewer)
			}
		})
	}

	// AND A CONTAINER THE TRACKER COULD NOT HOLD IS A BAD PARAMETER, not
	// an empty strip: a screen rendering nothing cannot tell a container
	// with no views from one that does not exist.
	for _, raw := range []string{"", "team:eng", "project:", "ENG"} {
		w := &stubWork{}
		_, err := askNative(t, queries.Sources{Work: w}, "work_views",
			map[string]any{"container": raw})
		if !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("container=%q answered %v, want a bad-parameter refusal", raw, err)
		}
	}
}

// THE PROJECTS LISTING'S TWO CLOSED SETS REACH THE READER AS THEMSELVES.
//
// `archived=` SELECTS a set and `sort=` orders the WHOLE of it before the
// engine's own cap takes a page. Both were the screen's own work once — the
// route sent a widening boolean and the directory narrowed and re-sorted what
// came back — so past the cap the Archived segment reported a company with
// dozens of retired projects as having none, and `sort=-open` ranked the most
// open work among the projects whose keys sort first.
func TestTheProjectsListingCarriesItsArchivalSetAndOrdering(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		archived   string
		sort       string
		wantSet    tracker.ArchivedMode
		wantSort   tracker.ProjectSort
		descending bool
	}{
		{"", "", tracker.ArchivedExclude, "", false},
		{"false", "key", tracker.ArchivedExclude, tracker.ProjectSortKey, false},
		{"only", "-active", tracker.ArchivedOnly, tracker.ProjectSortActive, true},
		{"true", "last_change", tracker.ArchivedInclude, tracker.ProjectSortLastChange, false},
	} {
		w := &stubWork{}
		args := map[string]any{}
		if tc.archived != "" {
			args["archived"] = tc.archived
		}
		if tc.sort != "" {
			args["sort"] = tc.sort
		}
		if _, err := askNative(t, queries.Sources{Work: w}, "work_projects",
			args); err != nil {
			t.Fatalf("work_projects%+v: %v", args, err)
		}
		if w.projectQuery.Archived != tc.wantSet {
			t.Errorf("archived=%q reached the reader as %q, want %q",
				tc.archived, w.projectQuery.Archived, tc.wantSet)
		}
		if w.projectQuery.Sort != tc.wantSort ||
			w.projectQuery.Descending != tc.descending {
			t.Errorf("sort=%q reached the reader as %q/%v, want %q/%v",
				tc.sort, w.projectQuery.Sort, w.projectQuery.Descending,
				tc.wantSort, tc.descending)
		}
	}

	// AND A VALUE THAT IS NEITHER SET'S IS A 400 NAMING THE PARAMETER, not
	// a silent fall back: a screen handed the default for a filter it
	// asked for draws a set nobody chose, and the reader has no way to
	// tell. `bad_params` is also the one code the dashboard renders as the
	// SCREEN's fault, so retrying is not offered.
	for _, tc := range []struct{ key, value, want string }{
		{"archived", "yes", "archived"},
		// `active` READS LIKE A VALUE and is not one — the live set is
		// `false`. Accepting it as the default would be exactly the
		// silent narrowing this refusal exists to stop.
		{"archived", "active", "archived"},
		{"sort", "lead", "sort"},
		{"sort", "-progress", "sort"},
	} {
		w := &stubWork{}
		_, err := askNative(t, queries.Sources{Work: w}, "work_projects",
			map[string]any{tc.key: tc.value})
		if !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%s=%s answered %v, want a bad-parameter refusal",
				tc.key, tc.value, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) ||
			!strings.Contains(err.Error(), tc.value) {
			t.Errorf("%s=%s is refused with %q, want the parameter and the "+
				"value named", tc.key, tc.value, err)
		}
		// AND THE READ IS NEVER MADE. A refusal that had already asked
		// the store would be a 400 over an answer somebody paid for.
		if w.projectQuery.Level != "" {
			t.Errorf("%s=%s reached the reader as %+v, want no read at all",
				tc.key, tc.value, w.projectQuery)
		}
	}
}

// A PROJECT THAT IS THERE IS NEVER ANSWERED AS MISSING. A `for_type` the
// company does not file was refused wrapped in tracker.ErrNoProject, so the
// route answered 404 about a project that exists — a dead link to a person,
// for an argument they could fix. The key that names nothing is still 404.
func TestAProjectDetailClassesWhatIsMissing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"no project", fmt.Errorf("tracker: no project \"ENGG\": %w", tracker.ErrNoProject),
			queries.ErrNotFound},
		{"no type", fmt.Errorf("tracker: no type \"epic\": %w", tracker.ErrNoType),
			queries.ErrBadParams},
	} {
		_, err := askNative(t, queries.Sources{Work: &stubWork{err: tc.err}},
			"work_project", map[string]any{"key": "ENG", "for_type": "epic"})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: answered %v, want %v", tc.name, err, tc.want)
		}
		if errors.Is(tc.want, queries.ErrBadParams) && errors.Is(err, queries.ErrNotFound) {
			t.Errorf("%s: an unknown type is also classed not-found", tc.name)
		}
	}
}

// A STRIP IS ONLY PERSONALISED BY A VIEWER THE CALLER MAY NAME.
//
// `viewer=` selects WHOSE pins and personal views order the strip, and nothing
// checked it: on a node with `api.allow_anonymous_read` a reader could take
// the handles out of `org` and page through every seat's pinned views. The
// scope rule is the one the other personal questions take — your own, or an
// operator credential for anybody else's.
//
// THE ABSENT CASE IS THE POINT OF THE SEPARATE RULE. A question ABOUT
// somebody refuses when the caller names nobody and has no seat; a strip is
// about a CONTAINER, so naming nobody is the shared strip, which is what the
// sidebar and the board poll for. Refusing that would have taken the strip off
// two screens for every reader whose token is not bound to a seat.
func TestAStripIsOnlyPersonalisedByAViewerTheCallerMayName(t *testing.T) {
	t.Parallel()

	// SOMEBODY ELSE'S, with no credential: refused, and as an
	// authorization failure rather than a bad parameter — the remedy is a
	// different credential, not a different handle.
	w := &stubWork{}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_views",
		map[string]any{"container": "workspace", "viewer": "ada-okonkwo"}); !errors.Is(
		err, queries.ErrUnauthorized) {

		t.Errorf("an anonymous caller naming another seat's handle answered %v, "+
			"want an authorization refusal", err)
	}
	// AND THE READER WAS NEVER ASKED, which is the half a refusal
	// returned after the read would not have bought.
	if w.views.Viewer.Named() {
		t.Errorf("the refused handle reached the reader as %+v", w.views.Viewer)
	}

	// AN OPERATOR NAMES ANYBODY'S: they hold the credential that writes
	// these records in the first place.
	w = &stubWork{}
	if _, err := askAsOperator(t, queries.Sources{Work: w}, "work_views",
		map[string]any{"container": "workspace", "viewer": "ada-okonkwo"}); err != nil {
		t.Fatalf("an operator naming a seat's handle: %v", err)
	}
	if w.views.Viewer.Handle != "ada-okonkwo" {
		t.Errorf("an operator's viewer reached the reader as %q, want ada-okonkwo",
			w.views.Viewer.Handle)
	}

	// AND NAMING NOBODY IS STILL THE SHARED STRIP, anonymously: a real
	// answer with an empty viewer, never a refusal.
	w = &stubWork{}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_views",
		map[string]any{"container": "workspace"}); err != nil {
		t.Fatalf("the shared strip, asked anonymously: %v", err)
	}
	if w.views.Viewer.Named() {
		t.Errorf("an unnamed viewer reached the reader as %+v, want empty", w.views.Viewer)
	}
}

// A GROUPED ANSWER REACHES THE CALLER.
//
// The payload was built by hand from `items` alone, and a grouped answer has
// NO flat rows by construction — so a populated board arrived as `items: []`
// and a screen rendered a company with no work.
func TestAGroupedAnswerReachesTheCaller(t *testing.T) {
	w := &stubWork{answer: tracker.Answer{
		Groups: []tracker.Group{{
			Key: "todo", Count: 12,
			Rows: []tracker.TaskRow{{ID: "t-1", Key: "ENG-1"}},
		}},
		GroupsDropped: 3, GroupsOverlap: true,
		Totals:   []tracker.Total{{Key: "points:sum", Column: "points", Op: "sum"}},
		Complete: true,
	}}
	got, err := askNative(t, queries.Sources{Work: w}, "work_items",
		map[string]any{"container": "project:eng", "group_by": "status"})
	if err != nil {
		t.Fatalf("work_items: %v", err)
	}
	payload, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("the answer is %T", got)
	}
	for _, key := range []string{"groups", "groups_dropped", "groups_overlap",
		"totals"} {
		if _, held := payload[key]; !held {
			t.Errorf("the payload carries no %q, so a board built from it "+
				"renders a company with no work", key)
		}
	}
}

// EVERY QUESTION ON THIS SURFACE RESOLVES THE CALLER'S OWN FRESHNESS, and the
// one that did not was twelve of the thirteen.
//
// `work_items` read `read_level` because it is the question that goes through
// the query grammar. Every other one wrote a hardcoded `stale` into its query
// and never looked at the key at all — so a caller asking a project listing
// for a linearizable answer was served this node's rows and told, in the
// answer's own `read_level` field, that it came back linearizable. A level
// that never downgrades silently is the whole contract; a surface that
// silently ignored the ask broke it through the other door.
//
// The case is written as a WALK over the questions rather than one assertion
// per reader, because the defect was never in one of them: it was that adding
// the fourteenth question would inherit whatever the thirteenth typed.
func TestEveryNativeQuestionResolvesTheCallersOwnLevel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		what  string
		args  map[string]any
		level func(*stubWork, *stubPages) statelog.ReadLevel
	}{
		{"work_items", map[string]any{},
			func(w *stubWork, _ *stubPages) statelog.ReadLevel { return w.query.Level }},
		{"work_item", map[string]any{"id": "ENG-1"},
			func(w *stubWork, _ *stubPages) statelog.ReadLevel { return w.taskLevel }},
		{"work_views", map[string]any{"container": "workspace"},
			func(w *stubWork, _ *stubPages) statelog.ReadLevel { return w.views.Level }},
		{"work_catalogue", map[string]any{},
			func(w *stubWork, _ *stubPages) statelog.ReadLevel { return w.catalogueQuery.Level }},
		{"work_person", map[string]any{"handle": "ana"},
			func(w *stubWork, _ *stubPages) statelog.ReadLevel { return w.personQuery.Level }},
		{"work_projects", map[string]any{},
			func(w *stubWork, _ *stubPages) statelog.ReadLevel { return w.projectQuery.Level }},
		{"work_project", map[string]any{"key": "ENG"},
			func(w *stubWork, _ *stubPages) statelog.ReadLevel { return w.detailQuery.Level }},
		{"work_activity", map[string]any{"container": "workspace"},
			func(w *stubWork, _ *stubPages) statelog.ReadLevel { return w.activityQuery.Level }},
		// OPERATOR-ONLY, and it is in this walk precisely because it
		// is: `my_work` is somebody's whole day, and an operator
		// reading it at a level nobody chose is the same defect with a
		// credential in front of it.
		{"work_my_work", map[string]any{"handle": "ana"},
			func(w *stubWork, _ *stubPages) statelog.ReadLevel { return w.myWorkQuery.Level }},
		{"work_inbox", map[string]any{"handle": "ana"},
			func(w *stubWork, _ *stubPages) statelog.ReadLevel { return w.inboxQuery.Level }},
		{"work_routing", map[string]any{"record_id": "r-1"},
			func(w *stubWork, _ *stubPages) statelog.ReadLevel { return w.routingQuery.Level }},
		{"pages", map[string]any{},
			func(_ *stubWork, p *stubPages) statelog.ReadLevel { return p.level }},
		{"page", map[string]any{"id": "p1"},
			func(_ *stubWork, p *stubPages) statelog.ReadLevel { return p.level }},
		{"containers", map[string]any{},
			func(_ *stubWork, p *stubPages) statelog.ReadLevel { return p.level }},
	} {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()
			// THE DEFAULT FIRST: a caller who says nothing gets this
			// surface's own level, not whatever the reader defaults to.
			work, pages := &stubWork{}, &stubPages{}
			src := queries.Sources{Work: work, Pages: pages}
			ask := askNative
			if personalQuestions[tc.what] {
				ask = askAsOperator
			}
			if _, err := ask(t, src, tc.what, tc.args); err != nil {
				t.Fatalf("%s: %v", tc.what, err)
			}
			want := statelog.DefaultReadLevel(statelog.SurfaceDashboard)
			if got := tc.level(work, pages); got != want {
				t.Errorf("%s read at %q with no ask, want this surface's "+
					"default %q", tc.what, got, want)
			}

			// AND THE ASK IS HONOURED, which is the half twelve of these
			// questions dropped on the floor.
			work, pages = &stubWork{}, &stubPages{}
			src = queries.Sources{Work: work, Pages: pages}
			asked := map[string]any{"read_level": "linearizable"}
			for k, v := range tc.args {
				asked[k] = v
			}
			if _, err := ask(t, src, tc.what, asked); err != nil {
				t.Fatalf("%s asking linearizable: %v", tc.what, err)
			}
			if got := tc.level(work, pages); got != statelog.ReadLinearizable {
				t.Errorf("%s asked for linearizable and read at %q — a level "+
					"the answer then reports back as the one asked for is a "+
					"stale answer wearing a stronger name", tc.what, got)
			}

			// AND A BARE `session` IS REFUSED RATHER THAN QUIETLY
			// SERVED: it waits for a position, and none was named.
			work, pages = &stubWork{}, &stubPages{}
			src = queries.Sources{Work: work, Pages: pages}
			bad := map[string]any{"read_level": "session"}
			for k, v := range tc.args {
				bad[k] = v
			}
			if _, err := ask(t, src, tc.what, bad); !errors.Is(err, queries.ErrBadParams) {
				t.Errorf("%s accepted a bare read_level=session, answering %v",
					tc.what, err)
			}

			// WITH THE POSITION IT IS HONOURED, which is the whole of
			// read-your-writes over this wire: a write answered with
			// where it landed and the caller hands that back.
			work, pages = &stubWork{}, &stubPages{}
			src = queries.Sources{Work: work, Pages: pages}
			own := map[string]any{
				"read_level": "session", "min_position": "CREWLET_TRACKER_LOG@1:4711",
			}
			for k, v := range tc.args {
				own[k] = v
			}
			if _, err := ask(t, src, tc.what, own); err != nil {
				t.Fatalf("%s asking session with a floor: %v", tc.what, err)
			}
			if got := tc.level(work, pages); got != statelog.ReadSession {
				t.Errorf("%s asked for session with a floor and read at %q",
					tc.what, got)
			}
		})
	}
}

// AND THE STALENESS BOUND REACHES THE READER, in both of its units.
//
// A bound refused where it is inconsistent and dropped where it is not is a
// bound that never bounded anything — the defect `max_lag_seconds` was fixed
// for on the board, and `max_lag_seq` was left in beside it, and which every
// other question here had in both units because none of them read either key.
// A tile declaring "at most twenty seconds behind" and rendering whatever came
// back is a live screen showing an hour-old answer.
func TestTheStalenessBoundsReachEveryQuestionThatCanHoldThem(t *testing.T) {
	t.Parallel()
	args := map[string]any{"max_lag_seconds": "30", "max_lag_seq": "250"}
	bounds := func(lag time.Duration, seq uint64) func(*testing.T, string) {
		return func(t *testing.T, what string) {
			t.Helper()
			if lag != 30*time.Second {
				t.Errorf("%s carried max_lag_seconds as %s", what, lag)
			}
			if seq != 250 {
				t.Errorf("%s carried max_lag_seq as %d — the record count is "+
					"the reading the broker actually answers", what, seq)
			}
		}
	}
	for _, tc := range []struct {
		what  string
		args  map[string]any
		check func(*stubWork, *stubPages) func(*testing.T, string)
	}{
		{"work_items", nil, func(w *stubWork, _ *stubPages) func(*testing.T, string) {
			return bounds(w.query.MaxLag, w.query.MaxLagSeq)
		}},
		// THE POINT READ AND THE PAGE READS TOO. They handed over the
		// level alone, on the claim that a single row has no set for a
		// bound to be enforced against — but the bound is about this
		// node's LAG, and a row is exactly as far behind as a board.
		{"work_item", map[string]any{"id": "ENG-1"},
			func(w *stubWork, _ *stubPages) func(*testing.T, string) {
				return bounds(w.taskFresh.MaxLag, w.taskFresh.MaxLagSeq)
			}},
		{"work_my_work", map[string]any{"handle": "ana"},
			func(w *stubWork, _ *stubPages) func(*testing.T, string) {
				return bounds(w.myWorkQuery.MaxLag, w.myWorkQuery.MaxLagSeq)
			}},
		{"work_inbox", map[string]any{"handle": "ana"},
			func(w *stubWork, _ *stubPages) func(*testing.T, string) {
				return bounds(w.inboxQuery.MaxLag, w.inboxQuery.MaxLagSeq)
			}},
		{"pages", nil, func(_ *stubWork, p *stubPages) func(*testing.T, string) {
			return bounds(p.fresh.MaxLag, p.fresh.MaxLagSeq)
		}},
		{"page", map[string]any{"id": "p1"},
			func(_ *stubWork, p *stubPages) func(*testing.T, string) {
				return bounds(p.fresh.MaxLag, p.fresh.MaxLagSeq)
			}},
		{"containers", nil, func(_ *stubWork, p *stubPages) func(*testing.T, string) {
			return bounds(p.fresh.MaxLag, p.fresh.MaxLagSeq)
		}},
		{"work_views", map[string]any{"container": "workspace"},
			func(w *stubWork, _ *stubPages) func(*testing.T, string) {
				return bounds(w.views.MaxLag, w.views.MaxLagSeq)
			}},
		{"work_catalogue", nil, func(w *stubWork, _ *stubPages) func(*testing.T, string) {
			return bounds(w.catalogueQuery.MaxLag, w.catalogueQuery.MaxLagSeq)
		}},
		{"work_person", map[string]any{"handle": "ana"},
			func(w *stubWork, _ *stubPages) func(*testing.T, string) {
				return bounds(w.personQuery.MaxLag, w.personQuery.MaxLagSeq)
			}},
		{"work_projects", nil, func(w *stubWork, _ *stubPages) func(*testing.T, string) {
			return bounds(w.projectQuery.MaxLag, w.projectQuery.MaxLagSeq)
		}},
		{"work_project", map[string]any{"key": "ENG"},
			func(w *stubWork, _ *stubPages) func(*testing.T, string) {
				return bounds(w.detailQuery.MaxLag, w.detailQuery.MaxLagSeq)
			}},
		{"work_activity", map[string]any{"container": "workspace"},
			func(w *stubWork, _ *stubPages) func(*testing.T, string) {
				return bounds(w.activityQuery.MaxLag, w.activityQuery.MaxLagSeq)
			}},
		{"work_routing", map[string]any{"record_id": "r-1"},
			func(w *stubWork, _ *stubPages) func(*testing.T, string) {
				return bounds(w.routingQuery.MaxLag, w.routingQuery.MaxLagSeq)
			}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()
			work, pages := &stubWork{}, &stubPages{}
			ask := askNative
			if personalQuestions[tc.what] {
				ask = askAsOperator
			}
			all := map[string]any{}
			for k, v := range args {
				all[k] = v
			}
			for k, v := range tc.args {
				all[k] = v
			}
			src := queries.Sources{Work: work, Pages: pages}
			if _, err := ask(t, src, tc.what, all); err != nil {
				t.Fatalf("%s: %v", tc.what, err)
			}
			tc.check(work, pages)(t, tc.what)

			// AND A BOUND AT A LEVEL THAT IS NOT STALE IS REFUSED
			// rather than carried and ignored: it is not a level of its
			// own and it narrows nothing at the other two, so passing
			// it there is a caller who believes they asked for
			// something they did not.
			all["read_level"] = "linearizable"
			if _, err := ask(t, src, tc.what, all); !errors.Is(err, queries.ErrBadParams) {
				t.Errorf("%s took a staleness bound at linearizable, answering %v",
					tc.what, err)
			}
		})
	}
}

// AND THE FLOOR REACHES EVERY QUESTION, at every level it may be asked at.
//
// `min_position` is what a write's answer carries, handed back: a caller that
// created a task through the operator MCP and redraws the board over this
// surface names the position and is served nothing from before it. A question
// that parsed the key and dropped it would answer identically — same rows,
// same `read_level` — which is the shape the staleness bounds already shipped
// in, so the walk is over every question rather than the one that goes
// through the grammar.
func TestTheCallersFloorReachesEveryNativeQuestion(t *testing.T) {
	t.Parallel()
	want := statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 4711}
	for _, tc := range []struct {
		what  string
		args  map[string]any
		floor func(*stubWork, *stubPages) statelog.Position
	}{
		{"work_items", nil, func(w *stubWork, _ *stubPages) statelog.Position { return w.query.MinPosition }},
		{"work_item", map[string]any{"id": "ENG-1"},
			func(w *stubWork, _ *stubPages) statelog.Position { return w.taskFresh.MinPosition }},
		{"work_views", map[string]any{"container": "workspace"},
			func(w *stubWork, _ *stubPages) statelog.Position { return w.views.MinPosition }},
		{"work_catalogue", nil, func(w *stubWork, _ *stubPages) statelog.Position { return w.catalogueQuery.MinPosition }},
		{"work_person", map[string]any{"handle": "ana"},
			func(w *stubWork, _ *stubPages) statelog.Position { return w.personQuery.MinPosition }},
		{"work_projects", nil, func(w *stubWork, _ *stubPages) statelog.Position { return w.projectQuery.MinPosition }},
		{"work_project", map[string]any{"key": "ENG"},
			func(w *stubWork, _ *stubPages) statelog.Position { return w.detailQuery.MinPosition }},
		{"work_activity", map[string]any{"container": "workspace"},
			func(w *stubWork, _ *stubPages) statelog.Position { return w.activityQuery.MinPosition }},
		{"work_my_work", map[string]any{"handle": "ana"},
			func(w *stubWork, _ *stubPages) statelog.Position { return w.myWorkQuery.MinPosition }},
		{"work_inbox", map[string]any{"handle": "ana"},
			func(w *stubWork, _ *stubPages) statelog.Position { return w.inboxQuery.MinPosition }},
		{"work_routing", map[string]any{"record_id": "r-1"},
			func(w *stubWork, _ *stubPages) statelog.Position { return w.routingQuery.MinPosition }},
		{"pages", nil, func(_ *stubWork, p *stubPages) statelog.Position { return p.fresh.MinPosition }},
		{"page", map[string]any{"id": "p1"},
			func(_ *stubWork, p *stubPages) statelog.Position { return p.fresh.MinPosition }},
		{"containers", nil, func(_ *stubWork, p *stubPages) statelog.Position { return p.fresh.MinPosition }},
	} {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()
			ask := askNative
			if personalQuestions[tc.what] {
				ask = askAsOperator
			}
			for _, level := range []string{"", "linearizable", "session", "stale", "consistent_prefix"} {
				work, pages := &stubWork{}, &stubPages{}
				src := queries.Sources{Work: work, Pages: pages}
				args := map[string]any{"min_position": want.String()}
				if level != "" {
					args["read_level"] = level
				}
				for k, v := range tc.args {
					args[k] = v
				}
				if _, err := ask(t, src, tc.what, args); err != nil {
					t.Fatalf("%s at read_level=%q with a floor: %v", tc.what, level, err)
				}
				if got := tc.floor(work, pages); got != want {
					t.Errorf("%s at read_level=%q carried the floor as %v, want %v "+
						"— a floor the reader never sees is one nothing waits for",
						tc.what, level, got, want)
				}
			}
			// AND A MALFORMED ONE IS REFUSED AS BAD PARAMETERS, naming
			// the key, rather than silently read as no floor.
			work, pages := &stubWork{}, &stubPages{}
			src := queries.Sources{Work: work, Pages: pages}
			args := map[string]any{"min_position": "4711"}
			for k, v := range tc.args {
				args[k] = v
			}
			if _, err := ask(t, src, tc.what, args); !errors.Is(err, queries.ErrBadParams) ||
				!strings.Contains(err.Error(), "min_position") {
				t.Errorf("%s took min_position=4711, answering %v", tc.what, err)
			}
		})
	}
}

// THE VIEWER IS A PROPERTY OF THE SURFACE, NOT A FILTER THE TRACKER PARSES.
//
// `workItems` read the viewer out of the parameters to build `tracker.Viewer`
// and then handed the WHOLE bag — `viewer` still in it — to `ExpandedQuery`,
// whose `ParseQuery` runs `checkKeys` over the merged map. `viewer` is not in
// `tracker.QueryKeys`, and `checkKeys` refuses a key nothing parses rather
// than ignoring it, so every `work_items` call that named a viewer came back
// `bad_params`.
//
// Two things that made it invisible. The refusal is a 400 with a message about
// an unknown parameter, which reads like the caller's fault; and the one
// surface that sends a viewer — the board's view strip — is also the one whose
// handler comment asserts the opposite, so the code documented the behaviour it
// did not have.
//
// It also made `preset=my_queue` unreachable here, which is the case below:
// the preset expands from `viewer.Handle` (internal/tracker/expand.go), so it
// is answerable only on a surface that HAS a viewer, and this was the surface.
func TestABoardMayNameItsViewer(t *testing.T) {
	w := &stubWork{}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_items", map[string]any{
		"viewer": "ada", "assignee": "ada",
	}); err != nil {
		t.Fatalf("work_items naming a viewer: %v", err)
	}
	// AND THE VIEWER STILL REACHED THE EXPANSION. Dropping the key is only
	// correct if the value survives as what it is — otherwise the fix would
	// be the bug's mirror, with `me` and every preset resolving to nobody.
	if got := w.query.Assignee; len(got) != 1 || got[0] != "ada" {
		t.Errorf("assignee = %v, want [ada]", got)
	}
}

// AND THE VIEWER STILL ARRIVES AS A VIEWER, which is the half a test on the
// refusal alone cannot see. Dropping the key is only correct if the value
// survives as what it is — the expansion resolves `f.<slug>=me` and every
// personal preset from `Viewer.Handle`, and a fix that deleted the key without
// threading the value would have made those resolve to nobody, which is a
// quieter wrong answer than the refusal it replaced.
//
// ASSERTED ON WHAT THE HANDLER PASSED rather than on a resolved `me`: `me` is
// a custom-field operator (`resolveViewerKeys` only substitutes `f.`-prefixed
// keys) and its resolution is the tracker's own, covered in that package. Here
// the question is whether this surface hands the viewer over at all.
func TestAViewerNamedByTheSurfaceReachesTheExpansion(t *testing.T) {
	w := &stubWork{}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_items", map[string]any{
		"viewer": "ada",
	}); err != nil {
		t.Fatalf("work_items naming a viewer: %v", err)
	}
	if got := w.expandViewer.Handle; got != "ada" {
		t.Errorf("expansion viewer = %q, want ada", got)
	}
}

// AN ACTOR KIND OFF THE WIRE IS CHECKED AT THIS SURFACE, and it is the only
// place that can check it.
//
// `work_activity` builds its `Kinds` by trusting whatever it was handed —
// [tracker.ChangeKind] over an arbitrary string — and an unknown change kind
// simply matches nothing, which is a filter that answers empty. That is
// survivable for a change kind and is not for the ACTOR kind, because it is
// the filter the audit screen is made of: an empty audit reads as a company
// nobody has touched, and a filter quietly ignored reads as one where
// everybody is an operator. The reader takes typed values and cannot tell a
// kind the caller invented from one this build was compiled without, so the
// refusal belongs here, at the edge where the string still exists.
func TestAnUnknownActorKindIsRefusedRatherThanFilteringToNothing(t *testing.T) {
	w := &stubWork{}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_activity", map[string]any{
		"container": "workspace", "actor_kinds": "root",
	}); err == nil {
		t.Fatal("work_activity accepted actor_kinds=root — nothing is written " +
			"under it, so the feed answers empty and the screen reads as a " +
			"company nobody has touched")
	}
	if got := w.activityQuery.ActorKinds; len(got) != 0 {
		t.Errorf("the refused query still reached the reader as %v", got)
	}

	// AND THE KINDS THE ENGINE DOES WRITE UNDER ALL ARRIVE, in the order
	// they were named — a gate that refused everything would pass the case
	// above and take the screen with it.
	if _, err := askNative(t, queries.Sources{Work: w}, "work_activity", map[string]any{
		"container": "workspace", "actor_kinds": "operator, human",
	}); err != nil {
		t.Fatalf("work_activity naming two real actor kinds: %v", err)
	}
	want := []tracker.AuthorKind{tracker.AuthorOperator, tracker.AuthorHuman}
	if got := w.activityQuery.ActorKinds; !slices.Equal(got, want) {
		t.Errorf("the reader was asked for %v, want %v", got, want)
	}
}
