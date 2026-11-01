package api

import (
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/authz"
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
// the grant check on the way.
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
	// One episode WHOLE, under the memory that lists its opening.
	{method: "GET", pattern: "/agents/{id}/memory/episodes/{episode}", what: "agent_episode", path: map[string]string{"id": "id", "episode": "episode"}},
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
	// EVERY SEAT'S TURNS over company days. A literal segment under
	// /agents, which the stream service serves exactly and nothing deeper.
	{method: "GET", pattern: "/agents/activity", what: "seat_activity"},
	{method: "GET", pattern: "/schedules", what: "schedules"},
	// ONE SCHEDULE'S OWN HISTORY. Three path segments because a schedule's
	// identity is all three — two units may each declare a "standup", and a
	// role and a unit may both — so a name alone would merge two teams'
	// histories into one list.
	{method: "GET", pattern: "/schedules/{scope_type}/{scope_id}/{name}/runs", what: "schedule_runs", path: map[string]string{"scope_type": "scope_type", "scope_id": "scope_id", "name": "name"}},
	{method: "GET", pattern: "/fleet", what: "fleet"},
	// THE BROKER'S MEMBERSHIP, beside the fleet view rather than inside it:
	// it asks a member for the metadata group, which the lease table the
	// fleet view is read from cannot say.
	{method: "GET", pattern: "/fleet/broker", what: "fleet_broker"},
	{method: "GET", pattern: "/sandbox-runs", what: "sandbox_runs"},
	// ONE RUNNING RUN'S LIVE OUTPUT, with the job as `launch_id=`: a run is
	// one execution of a turn and may launch more than one job.
	{method: "GET", pattern: "/sandbox-runs/{turn_id}/tail", what: "sandbox_tail", path: map[string]string{"turn_id": "turn_id"}},
	{method: "GET", pattern: "/budgets", what: "budgets"},
	{method: "GET", pattern: "/integrations", what: "integrations"},
	// WHAT EACH MCP SERVER DID ON EACH NODE. Not under /config/mcp-servers,
	// which is the configuration's own collection — this is what the fleet
	// did with it, from the heartbeats.
	{method: "GET", pattern: "/mcp-servers", what: "mcp_servers_status"},
	// WHAT THE FLEET HAS BACKED UP, and every backup a person asked for.
	// Plural beside `POST /backup`, which TAKES one: the read is a
	// collection and the write is one act, and neither is the other's
	// method on one path.
	{method: "GET", pattern: "/backups", what: "backups"},
	// The NATIVE backends. The literal segments beat the wildcards, as
	// above, so /work/counters is not read as an item whose key is
	// "counters" — net/http resolves the more specific pattern rather
	// than the first registered.
	{method: "GET", pattern: "/work/retention", what: "retention"},
	{method: "GET", pattern: "/work/projects/{key}", what: "work_project", path: map[string]string{"key": "key"}},
	{method: "GET", pattern: "/work/projects", what: "work_projects"},
	{method: "GET", pattern: "/work/workload", what: "work_workload"},
	// THE LANDING SCREEN'S SERIES AND WHAT WAITS ON THE CALLER, both
	// literal segments above /work/{id} for the reason given there.
	{method: "GET", pattern: "/work/flow", what: "work_flow"},
	{method: "GET", pattern: "/work/decisions", what: "decisions"},
	{method: "GET", pattern: "/work/activity", what: "work_activity"},
	{method: "GET", pattern: "/work/my-work", what: "work_my_work"},
	{method: "GET", pattern: "/work/inbox", what: "work_inbox"},
	// AND THE COMPOSED INBOX, every native domain's notices in one list —
	// no domain's path, since it is no domain's (queries' composedInbox).
	{method: "GET", pattern: "/inbox", what: "inbox"},
	// A TASK'S THREAD, deeper than the detail's own page, with the task as
	// `item=`. A literal segment rather than /work/{id}/comments, which
	// net/http refuses beside /work/projects/{key}: neither is more specific
	// for "/work/projects/comments", so the mux would not start.
	{method: "GET", pattern: "/work/comments", what: "work_comments"},
	// A TASK'S TURNS, beside its thread and for the same reason: a literal
	// segment with `id=` rather than /work/{id}/turns.
	{method: "GET", pattern: "/work/turns", what: "work_item_turns"},
	// SEARCH AND ROUTING, both above /work/{id} for the reason the comment
	// there gives: a literal segment beats the wildcard, so neither is read
	// as a task whose key is "search" or "routing".
	{method: "GET", pattern: "/work/search", what: "work_search"},
	{method: "GET", pattern: "/work/routing/{record_id}", what: "work_routing", path: map[string]string{"record_id": "record_id"}},
	{method: "GET", pattern: "/work/views", what: "work_views"},
	// A PROJECT'S FILES. The listing is a question like every other here;
	// one file's BYTES are not, and stream from the human write surface's
	// own route beside it (internal/api/workapi's files.go), which the
	// literal segment keeps apart from /work/{id}.
	{method: "GET", pattern: "/work/files", what: "work_files"},
	{method: "GET", pattern: "/work/views/saved", what: "work_saved_views"},
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
	// WHAT THE COMPANY DID, merged across the tracker, the pages and the
	// schedules — not under /work/, since two of its three sources are
	// not the tracker's.
	{method: "GET", pattern: "/feed", what: "company_feed"},
	// WHO IS ASKING. Not under /work/: the answer is the caller's own
	// identity rather than anything the tracker holds, and a node with no
	// native tracker still has a viewer.
	{method: "GET", pattern: "/viewer", what: "viewer"},
	// THE ARTIFACTS. The literals `/artifacts/usage` and `/artifacts/trail`
	// beat the `{id}` wildcard, so neither is read as an artifact of that id; every other
	// read names the artifact, and a version by its number. The writes, the
	// downloads and the source are the artifacts surface's own routes
	// (internal/api/artifactsapi), mounted beside these.
	{method: "GET", pattern: "/artifacts", what: "artifacts"},
	{method: "GET", pattern: "/artifacts/usage", what: "artifact_usage"},
	{method: "GET", pattern: "/artifacts/trail", what: "artifact_trail"},
	{method: "GET", pattern: "/artifacts/{id}", what: "artifact", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/artifacts/{id}/versions", what: "artifact_versions", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/artifacts/{id}/versions/{n}", what: "artifact_version", path: map[string]string{"id": "id", "n": "n"}},
	{method: "GET", pattern: "/artifacts/{id}/history", what: "artifact_history", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/artifacts/{id}/assets", what: "artifact_assets", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/artifacts/{id}/seen-by", what: "artifact_seen_by", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/artifacts/{id}/access", what: "artifact_access", path: map[string]string{"id": "id"}},
	// THE FOLDER TREE, under a root of its own: `/artifacts/folders` would
	// meet `/artifacts/{id}/pin`'s wildcard at `/artifacts/folders/pin`,
	// which the mux refuses to register.
	{method: "GET", pattern: "/artifact-folders", what: "artifact_folders"},
	// AN ARTIFACT'S DATA. The literal segments beat the collection
	// wildcard, so none is read as a collection of that name: each is a
	// name the datastore reserves (internal/artifactdata). The company's
	// datastore usage sits under /artifacts/data, a literal no artifact id
	// can be.
	{method: "GET", pattern: "/artifacts/data/usage", what: "artifact_data_usage"},
	{method: "GET", pattern: "/artifacts/{id}/data/schema", what: "artifact_data_schema", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/artifacts/{id}/data/history", what: "artifact_data_history", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/artifacts/{id}/data/state", what: "artifact_data_state", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/artifacts/{id}/data/{collection}/query", what: "artifact_data_query", path: map[string]string{"id": "id", "collection": "collection"}},
	{method: "GET", pattern: "/artifacts/{id}/data/{collection}/{doc}", what: "artifact_data_doc", path: map[string]string{"id": "id", "collection": "collection", "doc": "doc"}},
	{method: "GET", pattern: "/artifacts/{id}/data/{collection}", what: "artifact_data", path: map[string]string{"id": "id", "collection": "collection"}},
	// THE CODE HOST'S ROWS: the repositories, one repository in detail, and
	// one repository's push history, each decided per repository by the grant
	// the serving node derives (code.GrantOf, ADR-0058) — and who read a
	// unit's repository, an admin's, decided by its own verb. ON THESE PATHS
	// ALONE: they were the code host's own routes, mounted beside git
	// (code.go), and a question answered there too would be the second
	// implementation this table exists to prevent. The verify and its read
	// stay there: they are operations on THIS node's copy, not questions.
	{method: "GET", pattern: "/code/repos", what: "code_repos"},
	{method: "GET", pattern: "/code/repos/{name}", what: "code_repo", path: map[string]string{"name": "name"}},
	{method: "GET", pattern: "/code/repos/{name}/pushes", what: "code_pushes", path: map[string]string{"name": "name"}},
	{method: "GET", pattern: "/code/repos/{name}/readers", what: "code_repo_readers", path: map[string]string{"name": "name"}},
	// AND A CHANGE'S ROWS AND ITS ROUTING: the changes, one change a part at
	// a time, whom its records reached, and one reviewer's reviews — each
	// decided per repository as the rows are, a change named on its
	// repository's path by its number.
	{method: "GET", pattern: "/code/changes", what: "code_changes"},
	{method: "GET", pattern: "/code/repos/{name}/changes/{n}", what: "code_change", path: map[string]string{"name": "name", "n": "change"}},
	{method: "GET", pattern: "/code/repos/{name}/changes/{n}/routing", what: "code_change_routing", path: map[string]string{"name": "name", "n": "change"}},
	{method: "GET", pattern: "/code/reviews", what: "code_reviews"},
	// AND A REPOSITORY'S CONTENT: its refs, a directory, a file, a path's
	// history, a file's blame, a diff, a comparison with its merge preview
	// and one commit with the changes it belongs to — each read by object id
	// of the repository's home git node, and decided per repository as the
	// rows are.
	{method: "GET", pattern: "/code/repos/{name}/refs", what: "code_refs", path: map[string]string{"name": "name"}},
	{method: "GET", pattern: "/code/repos/{name}/tree", what: "code_tree", path: map[string]string{"name": "name"}},
	{method: "GET", pattern: "/code/repos/{name}/blob", what: "code_blob", path: map[string]string{"name": "name"}},
	{method: "GET", pattern: "/code/repos/{name}/log", what: "code_log", path: map[string]string{"name": "name"}},
	{method: "GET", pattern: "/code/repos/{name}/blame", what: "code_blame", path: map[string]string{"name": "name"}},
	{method: "GET", pattern: "/code/repos/{name}/diff", what: "code_diff", path: map[string]string{"name": "name"}},
	{method: "GET", pattern: "/code/repos/{name}/compare", what: "code_compare", path: map[string]string{"name": "name"}},
	{method: "GET", pattern: "/code/repos/{name}/commits/{sha}", what: "code_commit", path: map[string]string{"name": "name", "sha": "sha"}},
	// AND ITS MERGE QUEUES AND ITS CHECKS: the live queues, the attempts of
	// checks — a repository's, a change's, a commit's or every repository's
	// in a window — one attempt whole on its repository's path, and a page of
	// the log a failed run kept, each decided per repository as the rows are.
	// The branch an attempt was checked for rides the query as `target`: a
	// ref's slashes are no path segment's.
	{method: "GET", pattern: "/code/queue", what: "code_queue"},
	{method: "GET", pattern: "/code/checks", what: "code_checks"},
	{method: "GET", pattern: "/code/repos/{name}/checks/{oid}/{check}/{attempt}", what: "code_check", path: map[string]string{"name": "name", "oid": "oid", "check": "check", "attempt": "attempt"}},
	{method: "GET", pattern: "/code/repos/{name}/checks/{oid}/{check}/{attempt}/log", what: "code_check_log", path: map[string]string{"name": "name", "oid": "oid", "check": "check", "attempt": "attempt"}},
}

// mountReads registers the named read routes.
func (a *App) mountReads(mux httpjson.Router) {
	for _, route := range namedRoutes {
		mux.Handle(route.method+" "+route.pattern, a.serveNamed(route.what, route.path))
	}
	// The four served from the stream service rather than the registry.
	// They are the CONFIG-DERIVED surfaces plus the whole bundle, and the
	// socket builds them from these same functions for its handshake —
	// so a browser that cannot upgrade sees what one that could sees.
	//
	// EACH ONE IS DECIDED BY ITS PUSH KIND'S NEED AND SHAPED AS ITS PUSH
	// IS, from the same tables the socket reads (stream's
	// [stream.Audience]), so a fact is not reachable over REST by a caller
	// the socket would withhold it from: all four are the overview and
	// everybody's, and a seat's live call reaches a reader the seat's
	// trail is not open to as its status alone, exactly as the push hands
	// it to them.
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
// for a caller whose audience receives a push of kind, in the shape that push
// hands them.
//
// No registry entry, because there is no question to ask: these are the
// sections of the handshake snapshot, and the socket reads the same functions
// to assemble it. So they are decided by the same table the socket decides its
// pushes by — see [stream.Audience] — over the audience the socket would open
// with ([stream.Service.AudienceFor]), and what build returns is shaped for it
// ([stream.ShapeFor]), so the whole bundle carries only what that caller may
// read.
func (a *App) serveFrom(kind stream.Kind, build func(stream.Audience) any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.Caller(w, r)
		if !ok {
			return
		}
		out, err := a.overview(r.Context(), principal, kind, build)
		var refusal *queries.Refusal
		if errors.As(err, &refusal) {
			// THE SAME TWO FIELDS every refusal on authority carries,
			// rather than a sentence naming the permission: a client
			// reading `grants` off a refused question reads it off this
			// too.
			httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeUnauthorized,
				authz.RefusalDetail(refusal.Reason, refusal.Grants))
			return
		}
		writeJSON(w, http.StatusOK, out)
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
	writeAnswer(w, r, data)
}

// writeAnswer renders a question's answer, SHARED by every named route and the
// generic form for [writeQueryError]'s reason: as JSON, and — for an answer
// that is a function of the object ids its question named
// ([queries.Immutable]) — with the entity tag and the caching that say it never
// changes, answered `304` to a caller that already holds it
// ([httpjson.Immutable]). The socket sends the same answer as JSON alone:
// [queries.Immutable] encodes as what it wraps.
func writeAnswer(w http.ResponseWriter, r *http.Request, data any) {
	if answer, ok := data.(queries.Immutable); ok {
		httpjson.Immutable(w, r, answer.Answer)
		return
	}
	writeJSON(w, http.StatusOK, data)
}
