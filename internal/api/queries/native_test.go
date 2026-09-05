package queries_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/projection"
	"github.com/crewlet/crewlet/internal/work"
)

// stubWork and stubPages record what filter the surface built, which is the
// half a round trip through a real reader would hide: the questions here are
// almost entirely about turning a query string into a Filter, and a test that
// only checked the rows would pass with every filter dropped.
type stubWork struct {
	filter work.Filter
	items  []work.Summary
	detail work.Detail
	err    error
}

func (s *stubWork) List(_ context.Context, f work.Filter) ([]work.Summary, error) {
	s.filter = f
	return s.items, s.err
}

func (s *stubWork) Get(_ context.Context, _ string) (work.Detail, error) {
	return s.detail, s.err
}

func (s *stubWork) Counters(context.Context) (map[string]int, error) {
	return map[string]int{"ENG": 42}, s.err
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

// A PROJECTION THAT HAS NOT CAUGHT UP SAYS SO. Flattening it to an empty list
// would tell a person the company has no work — an answer they act on, by
// filing the duplicate or concluding the migration failed.
func TestAnUnhydratedProjectionIsUnavailableRatherThanEmpty(t *testing.T) {
	w := &stubWork{err: projection.ErrNotHydrated}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_items", nil); !errors.Is(err, queries.ErrUnavailable) {
		t.Errorf("an unhydrated board answered %v, want unavailable", err)
	}
	p := &stubPages{err: projection.ErrNotHydrated}
	if _, err := askNative(t, queries.Sources{Pages: p}, "pages", nil); !errors.Is(err, queries.ErrUnavailable) {
		t.Errorf("an unhydrated page listing answered %v, want unavailable", err)
	}
}

// A RECORD THAT IS NOT THERE IS NOT A FAILURE. A mistyped item key must read
// to an operator as a dead link, not as the server being broken.
func TestAMissingRecordIsNotFound(t *testing.T) {
	w := &stubWork{err: work.ErrNotFound}
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
		"project": "eng", "assignee": "swe", "label": "urgent",
		"status": "todo,in_progress", "q": "deploy", "limit": 10, "offset": 20,
	}); err != nil {
		t.Fatalf("work_items: %v", err)
	}
	got := w.filter
	// UPPERCASED, because a project key is compared upper everywhere else
	// and a board that only matched the case somebody typed would answer
	// empty for the same project spelled two ways.
	if got.Project != "ENG" {
		t.Errorf("project reached the reader as %q, want it upper-cased", got.Project)
	}
	if got.Assignee != "swe" || got.Label != "urgent" || got.Text != "deploy" {
		t.Errorf("a filter was dropped on the way: %+v", got)
	}
	if len(got.Status) != 2 || got.Status[0] != work.StatusTodo {
		t.Errorf("statuses reached the reader as %v", got.Status)
	}
	if got.Limit != 10 || got.Offset != 20 {
		t.Errorf("paging reached the reader as limit=%d offset=%d", got.Limit, got.Offset)
	}
}

// AN ABSENT `open` ASKS FOR EVERYTHING; open=false asks for the closed items.
// Reading an absent filter as false would make the default board show only
// finished work — which is the shape of bug a bool with no third state
// produces every time.
func TestOpenIsThreeStated(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]any
		want   *bool
	}{
		{name: "absent", params: nil},
		{name: "open", params: map[string]any{"open": true}, want: ptrTo(true)},
		{name: "closed", params: map[string]any{"open": false}, want: ptrTo(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &stubWork{}
			if _, err := askNative(t, queries.Sources{Work: w}, "work_items", tc.params); err != nil {
				t.Fatalf("work_items: %v", err)
			}
			switch {
			case tc.want == nil && w.filter.Open != nil:
				t.Errorf("an absent `open` reached the reader as %v", *w.filter.Open)
			case tc.want != nil && w.filter.Open == nil:
				t.Errorf("open=%v reached the reader as absent", *tc.want)
			case tc.want != nil && *w.filter.Open != *tc.want:
				t.Errorf("open=%v reached the reader as %v", *tc.want, *w.filter.Open)
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
	if msg := err.Error(); !strings.Contains(msg, "finished") || !strings.Contains(msg, string(work.StatusDone)) {
		t.Errorf("the refusal does not name the value and the set: %q", msg)
	}
}

// The board header's minted counters ride with the listing. A page's length
// is not a project's size, and a header that reported one as the other would
// say "50 items" for every project with more than fifty.
func TestTheBoardCarriesTheMintedCounters(t *testing.T) {
	got, err := askNative(t, queries.Sources{Work: &stubWork{}}, "work_items", nil)
	if err != nil {
		t.Fatalf("work_items: %v", err)
	}
	payload, _ := got.(map[string]any)
	minted, _ := payload["minted"].(map[string]int)
	if minted["ENG"] != 42 {
		t.Errorf("the board carried minted=%v", payload["minted"])
	}
}

func ptrTo[T any](v T) *T { return &v }
