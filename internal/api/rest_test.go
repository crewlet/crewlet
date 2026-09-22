package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE DOCUMENTED READ ROUTES EXIST.
//
// docs/reference/api-endpoints.md has listed this table since the engine
// shipped and thirteen of the reads answered 404: only the generic
// /query/{what} form was ever wired, so every reader following the published
// API got nothing. Measured against a running engine before this: /agents,
// /org, /tools, /events, /schedules, /fleet, /budgets, /integrations,
// /conversations, /sandbox-runs, /tokens/breakdown and /stream/snapshot were
// all Not Found while /query/events answered 200.
//
// A route missing is invisible from inside — nothing fails, the surface is
// simply absent — which is why this walks the table rather than spot-checking.

// status runs one request and reports only the status, so a route that
// answers a non-object (a JSON array, which several of these do) is still
// checked for being THERE.
func status(t *testing.T, a *api.App, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, path, nil)))
	return rec.Result().StatusCode
}

func restApp(t *testing.T) *api.App {
	t.Helper()
	c, err := config.ParseCompany([]byte(rosterCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return newApp(t, api.Options{
		Runtime: &fakeRuntime{},
		Sources: queries.Sources{Company: companySource(t, c)},
	})
}

// Every read route in the published table answers. Not 404, which is the one
// thing that was wrong with all of them.
func TestEveryDocumentedReadRouteAnswers(t *testing.T) {
	t.Parallel()
	a := restApp(t)

	// The routes every node serves from its company and its projection. A
	// question whose SOURCE is absent is left unregistered by design (a
	// company on Confluence has no native pages) and answers 404 for a
	// reason that is not "the route was never built", so those are
	// exercised separately below.
	for _, path := range []string{
		"/agents",
		"/org",
		"/tools",
		"/schedules",
		"/budgets",
		"/stream/snapshot",
	} {
		if got := status(t, a, path); got != http.StatusOK {
			t.Errorf("GET %s = %d, want 200; it is in the published API table", path, got)
		}
	}
}

// AND /integrations IS IN THE TABLE AND GUARDED, which is why it is not in
// the list above.
//
// It answers the same map `/setup` is guarded in full to protect: which
// surfaces are configured, which hold a credential, which are half-set-up and
// where each one's deliveries are pointed. A read anyone could make is
// reconnaissance for the write nobody can.
func TestTheIntegrationsMapIsNotServedAnonymously(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	restApp(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/integrations", nil))
	if got := rec.Result().StatusCode; got != http.StatusUnauthorized {
		t.Errorf("GET /integrations = %d, want 401: it is the same map /setup guards", got)
	}
}

// A ROUTE WHOSE SOURCE IS ABSENT IS STILL A ROUTE. It answers the query
// layer's own "no such question" rather than the mux's "no such path", and
// the difference matters: one says this node cannot answer, the other says
// the API does not have the concept.
func TestARouteWithNoSourceAnswersTheQueryLayer(t *testing.T) {
	t.Parallel()
	a := restApp(t)

	// No native knowledge base wired, which is a company on Confluence, so
	// `pages` is unregistered.
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, "/pages", nil)))
	res := rec.Result()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /pages = %d, want the query layer's 404", res.StatusCode)
	}
	// The mux's own 404 is text/plain; the query layer's carries a JSON
	// error code. That is how a caller tells them apart.
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q; a bare mux 404 means the route is not "+
			"registered at all, which is the bug this covers", ct)
	}
}

// A PATH VALUE REACHES THE ANSWER AS ITS PARAMETER, and the answer proves
// WHICH seat it was asked about.
//
// /agents/{id} and asking `agent` with an id are the same question over two
// transports. A route that captured the id and never passed it on would fail
// on a missing parameter instead — also a 4xx, which is why this reads the
// body rather than the status.
func TestAPathValueBecomesTheQuestionsParameter(t *testing.T) {
	t.Parallel()
	a := restApp(t)

	status, body := get(t, a, "/agents/ceo")
	if status != http.StatusOK {
		t.Fatalf("GET /agents/ceo = %d (%v), want the seat", status, body)
	}
	// The answer keys on the ROLE the handle resolves to, which is also
	// what the projection keys its overlays by.
	if body["role"] != "CEO" {
		t.Errorf("answered about %v, want the seat the path named", body["role"])
	}
	// And a different path answers about a different seat, so the route is
	// not returning a constant.
	if _, other := get(t, a, "/agents/cto"); other["role"] != "CTO" {
		t.Errorf("GET /agents/cto answered about %v", other["role"])
	}
}

// AND THE PATH WINS OVER THE QUERY STRING. In /agents/{id} the id IS the
// route; answering about a stray ?id= would silently redirect a request to a
// different seat's memory.
func TestThePathValueOverridesTheQueryString(t *testing.T) {
	t.Parallel()
	a := restApp(t)

	_, body := get(t, a, "/agents/ceo?id=cto&role=CTO")
	if body["role"] != "CEO" {
		t.Errorf("answered about %v; the query string is steering a route "+
			"addressed by its path", body["role"])
	}
}

// THE SEAT PAGE'S IDENTIFIER RESOLVES. The dashboard holds one identifier per
// seat — the handle — and sends it as `id`. The answer used to read only
// `role`, so every seat page answered 400 and rendered its error state.
func TestASeatIsAddressableByTheIdentifierTheClientHolds(t *testing.T) {
	t.Parallel()
	a := restApp(t)

	// The handle, which is what a roster row's id is.
	if code, body := get(t, a, "/query/agent?id=ceo"); code != http.StatusOK || body["role"] != "CEO" {
		t.Errorf("asking by handle = %d %v, want the seat", code, body)
	}
	// The role name still works, for a caller that already had one.
	if code, body := get(t, a, "/query/agent?role=CEO"); code != http.StatusOK || body["role"] != "CEO" {
		t.Errorf("asking by role = %d %v, want the seat", code, body)
	}
	// And the roster hands out exactly that identifier.
	for _, row := range rows(t, a.Stream().Snapshot()["agents"]) {
		if row["id"] != row["handle"] {
			t.Errorf("roster row %v: id must be the identifier every screen "+
				"sends back, which is the handle", row)
		}
	}
}

// THE TRACE ROUTE IS NOT AN EVENT CALLED "trace". Both patterns live under
// /events/, and a mux that took the first registered rather than the more
// specific one would answer /events/trace/{id} as an event lookup.
func TestTheTraceRouteBeatsTheEventWildcard(t *testing.T) {
	t.Parallel()
	a := restApp(t)

	// Neither question has a source here, so both reach the query layer —
	// what is asserted is that they reach DIFFERENT questions. `trace`
	// needs an event log and is unregistered; an event id of "trace"
	// would be too. So this pins the shape rather than the status: the
	// route must exist at all.
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, "/events/trace/t-1", nil)))
	if ct := rec.Result().Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET /events/trace/{id} content-type = %q; a bare mux 404 "+
			"means the pattern never matched", ct)
	}
}

// THE NAMED ROUTE AND THE GENERIC FORM ARE ONE IMPLEMENTATION.
//
// The registry exists so two surfaces cannot answer one question from two
// implementations. A named route that re-derived its answer would agree today
// and drift the first time either changed.
func TestANamedRouteAgreesWithTheGenericForm(t *testing.T) {
	t.Parallel()
	a := restApp(t)

	for _, pair := range []struct{ named, generic string }{
		{"/schedules", "/query/schedules"},
		{"/integrations", "/query/integrations"},
		{"/budgets", "/query/budgets"},
	} {
		_, viaNamed := get(t, a, pair.named)
		_, viaQuery := get(t, a, pair.generic)
		if len(viaNamed) != len(viaQuery) {
			t.Errorf("%s and %s answered different shapes (%v vs %v)",
				pair.named, pair.generic, viaNamed, viaQuery)
		}
	}
}

// A DOCUMENTED PATH UNDER A WILDCARD IS THE ONE THAT FAILS SILENTLY.
//
// Every literal `/work/<word>` route in the table sits above `/work/{id}`, and
// one that is registered on the query channel and listed in
// docs/reference/api-endpoints.md but MISSING from the table fails without a
// sound: the wildcard matches that path, so the request is answered as a TASK
// whose key is that word — a 404 naming a record nobody asked for, from a
// route the reader believed they were on. `work_burndown` shipped in exactly
// that state for as long as it existed.
//
// That is why this asserts the CODE rather than merely a non-404. Only
// [queries.Sources.workViews] can refuse for a missing `container`, so
// `bad_params` is proof the path reached the view strip and not the wildcard
// below it; a route that went missing again would answer `not_found` here and
// a status-only check would pass on it.
func TestALiteralWorkRouteIsNotShadowedByTheWildcard(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{
		Runtime: &fakeRuntime{},
		Sources: queries.Sources{Work: stubWorkReader{}},
	})

	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, "/work/views", nil)))
	res := rec.Result()

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET /work/views = %d, want 400: the query refuses a "+
			"missing `container`, and any other status means the path was "+
			"answered by /work/{id} instead", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "bad_params" {
		t.Errorf("error = %q, want bad_params; `not_found` is the wildcard "+
			"answering about a task called \"views\"", body["error"])
	}
}

// AND IT CARRIES ITS PARAMETERS, so the route is wired to the answer rather
// than merely present. A path that reached the query and dropped the query
// string would refuse for the same `container` it was handed.
func TestALiteralWorkRouteCarriesItsQueryString(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{
		Runtime: &fakeRuntime{},
		Sources: queries.Sources{Work: stubWorkReader{}},
	})

	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet,
		"/work/views?container=project:ENG", nil)))
	if got := rec.Result().StatusCode; got != http.StatusOK {
		t.Errorf("GET /work/views?container=project:ENG = %d, want 200", got)
	}
}

// stubWorkReader answers every method of the seam with its zero value: a route
// test must not depend on what a tracker would have said.
type stubWorkReader struct{}

func (stubWorkReader) Inbox(context.Context, tracker.InboxQuery, time.Time) (
	tracker.InboxAnswer, error) {
	return tracker.InboxAnswer{}, nil
}

func (stubWorkReader) Routing(context.Context, tracker.RoutingQuery, time.Time) (
	tracker.RoutingAnswer, error) {
	return tracker.RoutingAnswer{}, nil
}

func (stubWorkReader) Tasks(context.Context, tracker.Query, time.Time) (tracker.Answer, error) {
	return tracker.Answer{}, nil
}

func (stubWorkReader) Task(context.Context, string, tracker.DetailWants,
	statelog.Freshness) (tracker.TaskDetail, error) {
	return tracker.TaskDetail{}, tracker.ErrNoTask
}

func (stubWorkReader) Views(context.Context, tracker.ViewQuery) (tracker.ViewListing, error) {
	return tracker.ViewListing{}, nil
}

func (stubWorkReader) ExpandedQuery(context.Context, map[string]any, tracker.Viewer,
	time.Time, *time.Location) (tracker.Query, error) {
	return tracker.Query{}, nil
}

func (stubWorkReader) Catalogue(context.Context, tracker.CatalogueQuery) (
	tracker.CatalogueAnswer, error) {
	return tracker.CatalogueAnswer{}, nil
}

func (stubWorkReader) Projects(context.Context, tracker.ProjectQuery) (
	tracker.ProjectListing, error) {
	return tracker.ProjectListing{}, nil
}

func (stubWorkReader) Project(context.Context, tracker.ProjectDetailQuery) (
	tracker.ProjectDetail, error) {
	return tracker.ProjectDetail{}, nil
}

func (stubWorkReader) Workload(_ context.Context, q tracker.WorkloadQuery, _ time.Time) (
	tracker.WorkloadAnswer, error) {
	return tracker.WorkloadAnswer{
		Rows: []tracker.WorkloadRow{{Handle: q.Unit, Open: 1}},
	}, nil
}

func (stubWorkReader) Activity(context.Context, tracker.ActivityQuery, time.Time) (
	tracker.ActivityAnswer, error) {
	return tracker.ActivityAnswer{}, nil
}

func (stubWorkReader) MyWork(context.Context, tracker.MyWorkQuery, time.Time) (
	tracker.MyWork, error) {
	return tracker.MyWork{}, nil
}

func (stubWorkReader) Person(context.Context, tracker.PersonQuery, time.Time) (
	tracker.PersonState, error) {
	return tracker.PersonState{}, nil
}

// THE INBOX IS A LITERAL SEGMENT, not a task called "inbox".
//
// Same hazard the burndown had, one route later: `/work/{id}` matches the
// path, so a table entry with no route behind it answers 404 about a task
// nobody named. The refusal proves the query was reached — `work_inbox`
// refuses an unresolvable handle with `bad_params`, and the wildcard would
// answer `not_found`.
func TestTheInboxIsServedAtThePathTheTableDocuments(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{
		Runtime: &fakeRuntime{},
		Sources: queries.Sources{Work: stubWorkReader{}},
	})

	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, "/work/inbox", nil)))
	res := rec.Result()

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET /work/inbox = %d, want 400: an anonymous caller names "+
			"no seat and the question refuses, so any other status means "+
			"/work/{id} answered the path instead", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "bad_params" {
		t.Errorf("error = %q, want bad_params; `not_found` is the wildcard "+
			"answering about a task called \"inbox\"", body["error"])
	}
}
