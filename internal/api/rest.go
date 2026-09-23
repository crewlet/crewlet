package api

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
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
	// One seat's external threads. Under /agents/{id}/ beside its memory,
	// because it is the same kind of fact — what this seat has said and
	// remembered — rather than a company-wide listing.
	{method: "GET", pattern: "/agents/{id}/conversations", what: "conversations", path: map[string]string{"id": "handle"}},
	{method: "GET", pattern: "/agents/{id}", what: "agent", path: map[string]string{"id": "id"}},
	// The literal segment beats the wildcard, so /events/trace/{id} is not
	// read as an event whose id is "trace" — net/http resolves the more
	// specific pattern rather than the first registered.
	{method: "GET", pattern: "/events/trace/{trace_id}", what: "trace", path: map[string]string{"trace_id": "trace_id"}},
	// THE LOG'S OWN TIME AXIS, beside the listing rather than a parameter
	// of it: the two answers have different shapes, and one route returning
	// either would make every caller branch on what came back. The literal
	// segment beats the wildcard below, so this is not read as an event
	// whose id is "series".
	{method: "GET", pattern: "/events/series", what: "event_series"},
	{method: "GET", pattern: "/events/{id}", what: "event", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/events", what: "events"},
	// THE LIST OF TURNS. Not under /events/: a turn is not an event, and
	// filing it there would put the unit of work under the log that
	// records it.
	{method: "GET", pattern: "/turns", what: "turns"},
	{method: "GET", pattern: "/tokens/breakdown", what: "tokens"},
	// THE SAME SPEND WITH A TIME AXIS. Beside the breakdown rather than a
	// parameter of it: the two answers have different shapes, and one route
	// returning either would make every caller branch on what came back.
	{method: "GET", pattern: "/tokens/series", what: "token_series"},
	{method: "GET", pattern: "/schedules", what: "schedules"},
	// ONE SCHEDULE'S OWN HISTORY. Three path segments because a schedule's
	// identity is all three — two units may each declare a "standup", and a
	// role and a unit may both — so a name alone would merge two teams'
	// histories into one list.
	{method: "GET", pattern: "/schedules/{scope_type}/{scope_id}/{name}/runs", what: "schedule_runs", path: map[string]string{"scope_type": "scope_type", "scope_id": "scope_id", "name": "name"}},
	{method: "GET", pattern: "/fleet", what: "fleet"},
	{method: "GET", pattern: "/sandbox-runs", what: "sandbox_runs"},
	{method: "GET", pattern: "/budgets", what: "budgets"},
	{method: "GET", pattern: "/integrations", what: "integrations"},
	// The NATIVE backends. The literal segments beat the wildcards, as
	// above, so /work/counters is not read as an item whose key is
	// "counters" — net/http resolves the more specific pattern rather
	// than the first registered.
	{method: "GET", pattern: "/work/retention", what: "retention"},
	{method: "GET", pattern: "/work/projects/{key}", what: "work_project", path: map[string]string{"key": "key"}},
	{method: "GET", pattern: "/work/projects", what: "work_projects"},
	{method: "GET", pattern: "/work/workload", what: "work_workload"},
	{method: "GET", pattern: "/work/activity", what: "work_activity"},
	{method: "GET", pattern: "/work/my-work", what: "work_my_work"},
	{method: "GET", pattern: "/work/inbox", what: "work_inbox"},
	// SEARCH AND ROUTING, both above /work/{id} for the reason the comment
	// there gives: a literal segment beats the wildcard, so neither is read
	// as a task whose key is "search" or "routing".
	{method: "GET", pattern: "/work/search", what: "work_search"},
	{method: "GET", pattern: "/work/routing/{record_id}", what: "work_routing", path: map[string]string{"record_id": "record_id"}},
	{method: "GET", pattern: "/work/views", what: "work_views"},
	{method: "GET", pattern: "/work/catalogue", what: "work_catalogue"},
	{method: "GET", pattern: "/work/people/{handle}", what: "work_person", path: map[string]string{"handle": "handle"}},
	{method: "GET", pattern: "/work/{id}", what: "work_item", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/work", what: "work_items"},
	// The page ACTIVITY and one REVISION's body. Both above /pages/{id},
	// for the reason the work routes give: a literal segment beats the
	// wildcard, so neither is read as a page whose id is "activity".
	{method: "GET", pattern: "/pages/activity", what: "page_activity"},
	{method: "GET", pattern: "/pages/{id}/revisions/{version}", what: "page_revision", path: map[string]string{"id": "page", "version": "version"}},
	{method: "GET", pattern: "/pages/{id}", what: "page", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/pages", what: "pages"},
	{method: "GET", pattern: "/containers", what: "containers"},
	// WHO IS ASKING. Not under /work/: the answer is the caller's own
	// identity rather than anything the tracker holds, and a node with no
	// native tracker still has a viewer.
	{method: "GET", pattern: "/viewer", what: "viewer"},
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
	//
	// EACH ONE IS DECIDED BY ITS PUSH KIND'S GRANT, the same table the
	// socket reads (stream's [stream.Audience]), so a fact is not reachable
	// over REST by a caller the socket would refuse it to — they used to be
	// served to any resolved caller at all.
	mux.Handle("GET /agents", a.serveFrom(stream.KindAgents,
		func(stream.Audience) any { return a.stream.Roster() }))
	mux.Handle("GET /org", a.serveFrom(stream.KindOrg,
		func(stream.Audience) any { return a.stream.Org() }))
	mux.Handle("GET /tools", a.serveFrom(stream.KindTools,
		func(stream.Audience) any { return a.stream.Tools() }))
	mux.Handle("GET /stream/snapshot", a.serveFrom(stream.KindSnapshot,
		func(audience stream.Audience) any { return a.stream.Snapshot(audience) }))
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

// serveFrom renders a value the stream service already knows how to build,
// for a caller whose grants let them receive a push of kind.
//
// No registry entry, because there is no question to ask: these are the
// sections of the handshake snapshot, and the socket reads the same functions
// to assemble it. So they are decided by the same table the socket decides its
// pushes by — see [stream.Audience] — and build is handed the caller's
// audience, so the whole bundle carries only what that caller may read.
func (a *App) serveFrom(kind string, build func(stream.Audience) any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.Caller(w, r)
		if !ok {
			return
		}
		audience := stream.AudienceOf(principal.Grants)
		if !audience.Receives(kind) {
			grant, _ := stream.GrantFor(kind)
			httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeUnauthorized,
				map[string]string{"detail": "this needs " + string(grant)})
			return
		}
		writeJSON(w, http.StatusOK, build(audience))
	}
}

// answerHTTP runs a question and renders it, sharing serveQuery's error
// mapping so a named route and the generic form cannot answer one failure two
// ways.
func (a *App) answerHTTP(w http.ResponseWriter, r *http.Request, what string, params queries.Params) {
	// THE PRINCIPAL TRAVELS IN THE CONTEXT and nothing is passed beside
	// it: the registry reads [iam.From] for the grant and every personal
	// question reads it for the caller, so there is no second, converted
	// identity for the two to disagree about.
	data, err := a.queries.AnswerWith(r.Context(), what, params)
	if err != nil {
		writeQueryError(w, what, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}
