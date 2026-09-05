package api

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/queries"
)

// The named read routes — the public REST API.
//
// EVERY ONE OF THEM IS AN ADAPTER, never a second implementation. A named
// route resolves its path values, hands them to the SAME registry entry the
// socket's query channel reaches, and renders the same answer; the four that
// are not registry questions read the SAME functions that build the socket's
// handshake snapshot. Two surfaces answering one question from two
// implementations is how they end up disagreeing with nobody noticing, and
// the registry exists so a route cannot read its own parameters and forget
// the operator check on the way.
//
// They were documented before they were built. docs/reference/api-endpoints.md
// has listed this table since the engine shipped and thirteen of the reads
// answered 404 — a public API that only the generic /query/{what} form
// actually served. The paths here are that document's, so the two now agree.

// namedRoutes maps a path to the registry question behind it.
//
// The path values a route captures are named too, because that is the whole
// difference between /agents/{id} and asking `agent` with an id: the route
// carries it in the URL and the query channel carries it in a JSON object,
// and both have to arrive at the answer as the same parameter.
var namedRoutes = []struct {
	method  string
	pattern string
	what    string

	// path maps a route wildcard to the parameter it supplies.
	path map[string]string
}{
	{method: "GET", pattern: "/agents/{id}/memory", what: "agent_memory", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/agents/{id}", what: "agent", path: map[string]string{"id": "id"}},
	// The literal segment beats the wildcard, so /events/trace/{id} is not
	// read as an event whose id is "trace" — net/http resolves the more
	// specific pattern rather than the first registered.
	{method: "GET", pattern: "/events/trace/{trace_id}", what: "trace", path: map[string]string{"trace_id": "trace_id"}},
	{method: "GET", pattern: "/events/{id}", what: "event", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/events", what: "events"},
	{method: "GET", pattern: "/tokens/breakdown", what: "tokens"},
	{method: "GET", pattern: "/schedules", what: "schedules"},
	{method: "GET", pattern: "/fleet", what: "fleet"},
	{method: "GET", pattern: "/sandbox-runs", what: "sandbox_runs"},
	{method: "GET", pattern: "/budgets", what: "budgets"},
	{method: "GET", pattern: "/integrations", what: "integrations"},
	// The NATIVE backends. The literal segments beat the wildcards, as
	// above, so /work/counters is not read as an item whose key is
	// "counters" — net/http resolves the more specific pattern rather
	// than the first registered.
	{method: "GET", pattern: "/work/{id}", what: "work_item", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/work", what: "work_items"},
	{method: "GET", pattern: "/pages/{id}", what: "page", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/pages", what: "pages"},
	{method: "GET", pattern: "/containers", what: "containers"},
}

// mountReads registers the named read routes.
func (a *App) mountReads(mux *http.ServeMux) {
	for _, route := range namedRoutes {
		mux.Handle(route.method+" "+route.pattern, a.serveNamed(route.what, route.path))
	}
	// The four served from the stream service rather than the registry.
	// They are the CONFIG-DERIVED surfaces plus the whole bundle, and the
	// socket builds them from these same functions for its handshake —
	// so a browser that cannot upgrade sees what one that could sees.
	mux.Handle("GET /agents", a.serveFrom(func() any { return a.stream.Roster() }))
	mux.Handle("GET /org", a.serveFrom(func() any { return a.stream.Org() }))
	mux.Handle("GET /tools", a.serveFrom(func() any { return a.stream.Tools() }))
	mux.Handle("GET /stream/snapshot", a.serveFrom(func() any { return a.stream.Snapshot() }))
}

// serveNamed answers one registry question from a named route.
func (a *App) serveNamed(what string, path map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params := queries.FromQuery(r.URL.Query())
		for wildcard, param := range path {
			if value := r.PathValue(wildcard); value != "" {
				params = params.With(param, value)
			}
		}
		a.answerHTTP(w, r, what, params)
	}
}

// serveFrom renders a value the stream service already knows how to build.
//
// No registry entry, because there is no question to ask: these are the
// sections of the handshake snapshot, and the socket reads the same functions
// to assemble it. Guarded like every other read — the mux is wrapped whole.
func (a *App) serveFrom(build func() any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, build())
	}
}

// answerHTTP runs a question and renders it, sharing serveQuery's error
// mapping so a named route and the generic form cannot answer one failure two
// ways.
func (a *App) answerHTTP(w http.ResponseWriter, r *http.Request, what string, params queries.Params) {
	operatorID, _ := auth.OperatorFrom(r.Context())
	data, err := a.queries.AnswerWith(r.Context(), what, params, operatorID)
	if err != nil {
		writeQueryError(w, what, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}
