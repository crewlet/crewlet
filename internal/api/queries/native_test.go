package queries_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// stubWork and stubPages record what filter the surface built, which is the
// half a round trip through a real reader would hide: the questions here are
// almost entirely about turning a query string into a Filter, and a test that
// only checked the rows would pass with every filter dropped.
type stubWork struct {
	query       tracker.Query
	answer      tracker.Answer
	detail      tracker.TaskDetail
	views       tracker.ViewQuery
	listing     tracker.ViewListing
	goalQuery   tracker.GoalQuery
	goals       tracker.GoalListing
	personQuery tracker.PersonQuery
	person      tracker.PersonState

	projectQuery tracker.ProjectQuery
	projects     tracker.ProjectListing
	detailQuery  tracker.ProjectDetailQuery
	project      tracker.ProjectDetail
	sprintQuery  tracker.SprintQuery
	sprints      tracker.SprintListing

	err error
}

func (s *stubWork) Projects(_ context.Context, q tracker.ProjectQuery,
	_ time.Time) (tracker.ProjectListing, error) {

	s.projectQuery = q
	return s.projects, s.err
}

func (s *stubWork) Project(_ context.Context, q tracker.ProjectDetailQuery,
	_ time.Time) (tracker.ProjectDetail, error) {

	s.detailQuery = q
	return s.project, s.err
}

func (s *stubWork) Sprints(_ context.Context, q tracker.SprintQuery,
	_ time.Time) (tracker.SprintListing, error) {

	s.sprintQuery = q
	return s.sprints, s.err
}

func (s *stubWork) Views(_ context.Context, q tracker.ViewQuery) (tracker.ViewListing, error) {
	s.views = q
	return s.listing, s.err
}

func (s *stubWork) ExpandedQuery(_ context.Context, params map[string]any,
	_ string, now time.Time, loc *time.Location) (tracker.Query, error) {

	return tracker.ParseQuery(tracker.MapParams(params), now, loc)
}

func (s *stubWork) Goals(_ context.Context, q tracker.GoalQuery) (tracker.GoalListing, error) {
	s.goalQuery = q
	return s.goals, s.err
}

func (s *stubWork) Catalogue(context.Context, tracker.CatalogueQuery) (tracker.CatalogueAnswer, error) {
	return tracker.CatalogueAnswer{}, nil
}

func (s *stubWork) Person(_ context.Context, q tracker.PersonQuery, _ time.Time) (tracker.PersonState, error) {
	s.personQuery = q
	return s.person, s.err
}

func (s *stubWork) Tasks(_ context.Context, q tracker.Query, _ time.Time) (tracker.Answer, error) {
	s.query = q
	return s.answer, s.err
}

func (s *stubWork) Task(_ context.Context, _ string, _ tracker.DetailWants,
	_ statelog.ReadLevel) (tracker.TaskDetail, error) {

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
}

func (s *stubPages) List(_ context.Context, f pages.Filter,
	level statelog.ReadLevel,
) (pages.Listing, error) {
	s.filter, s.level = f, level
	return pages.Listing{Pages: s.list, Level: level, Complete: true}, s.err
}

func (s *stubPages) Get(_ context.Context, _ string,
	level statelog.ReadLevel,
) (pages.Detail, error) {
	s.level = level
	return pages.Detail{}, s.err
}

func (s *stubPages) Containers(_ context.Context,
	level statelog.ReadLevel,
) ([]pages.Container, error) {
	s.level = level
	return nil, s.err
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
			if _, err := askNative(t, queries.Sources{Work: w}, "work_views",
				map[string]any{"container": tc.raw, "viewer": "ana"}); err != nil {
				t.Fatalf("work_views: %v", err)
			}
			if w.views.Container.Kind != tc.kind || w.views.Container.ID != tc.id {
				t.Fatalf("%q reached the reader as %s %q, want %s %q", tc.raw,
					w.views.Container.Kind, w.views.Container.ID, tc.kind, tc.id)
			}
			if w.views.Viewer != "ana" {
				t.Fatalf("the viewer reached the reader as %q", w.views.Viewer)
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
