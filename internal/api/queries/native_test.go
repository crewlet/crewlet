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
	query  tracker.Query
	answer tracker.Answer
	detail tracker.TaskDetail
	err    error
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
}

func (s *stubPages) List(_ context.Context, f pages.Filter) ([]pages.Summary, error) {
	s.filter = f
	return s.list, s.err
}

func (s *stubPages) Get(context.Context, string) (pages.Detail, error) {
	return pages.Detail{}, s.err
}

func (s *stubPages) Containers(context.Context) ([]pages.Container, error) {
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
