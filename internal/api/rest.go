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
	// WHERE THE COMPANY'S APPS LIVE IN A BROWSER, everybody's where
	// `/integrations` is an administrator's: a kind and a base address per app.
	{method: "GET", pattern: "/link-hosts", what: "link_hosts"},
	// WHAT EACH MCP SERVER DID ON EACH NODE. Not under /config/mcp-servers,
	// which is the configuration's own collection — this is what the fleet
	// did with it, from the heartbeats.
	{method: "GET", pattern: "/mcp-servers", what: "mcp_servers_status"},
	// WHAT EACH MODEL IS DOING, and what Settings › Models offers: the
	// runners a model can be placed on, the models a step suggests, and one
	// device sign-in while its record lingers. The writes beside them are
	// llmapi's own routes under /llm/providers/.
	{method: "GET", pattern: "/llm/entries", what: "llm_entries"},
	{method: "GET", pattern: "/llm/runners", what: "llm_runners"},
	{method: "GET", pattern: "/llm/models", what: "llm_models"},
	{method: "GET", pattern: "/llm/device-sign-ins/{id}", what: "llm_device_sign_in", path: map[string]string{"id": "id"}},
	// WHAT THE FLEET HAS BACKED UP, and every backup a person asked for.
	// Plural beside `POST /backup`, which TAKES one: the read is a
	// collection and the write is one act, and neither is the other's
	// method on one path.
	{method: "GET", pattern: "/backups", what: "backups"},
	// The NATIVE backends. The literal segments beat the wildcards, as
	// above, so /worklet/counters is not read as an item whose key is
	// "counters" — net/http resolves the more specific pattern rather
	// than the first registered.
	{method: "GET", pattern: "/worklet/retention", what: "retention"},
	{method: "GET", pattern: "/worklet/projects/{key}", what: "work_project", path: map[string]string{"key": "key"}},
	{method: "GET", pattern: "/worklet/projects", what: "work_projects"},
	{method: "GET", pattern: "/worklet/workload", what: "work_workload"},
	// THE LANDING SCREEN'S SERIES AND WHAT WAITS ON THE CALLER, both
	// literal segments above /worklet/{id} for the reason given there.
	{method: "GET", pattern: "/worklet/flow", what: "work_flow"},
	{method: "GET", pattern: "/worklet/decisions", what: "decisions"},
	{method: "GET", pattern: "/worklet/decisions/answered", what: "answered_decisions"},
	{method: "GET", pattern: "/worklet/activity", what: "work_activity"},
	{method: "GET", pattern: "/worklet/my-work", what: "work_my_work"},
	{method: "GET", pattern: "/worklet/inbox", what: "work_inbox"},
	// A TASK'S THREAD, deeper than the detail's own page, with the task as
	// `item=`. A literal segment rather than /worklet/{id}/comments, which
	// net/http refuses beside /worklet/projects/{key}: neither is more specific
	// for "/worklet/projects/comments", so the mux would not start.
	{method: "GET", pattern: "/worklet/comments", what: "work_comments"},
	// A TASK'S TURNS, beside its thread and for the same reason: a literal
	// segment with `id=` rather than /worklet/{id}/turns.
	{method: "GET", pattern: "/worklet/turns", what: "work_item_turns"},
	// SEARCH AND ROUTING, both above /worklet/{id} for the reason the comment
	// there gives: a literal segment beats the wildcard, so neither is read
	// as a task whose key is "search" or "routing".
	{method: "GET", pattern: "/worklet/search", what: "work_search"},
	{method: "GET", pattern: "/worklet/routing/{record_id}", what: "work_routing", path: map[string]string{"record_id": "record_id"}},
	{method: "GET", pattern: "/worklet/views", what: "work_views"},
	// A PROJECT'S FILES. The listing is a question like every other here;
	// one file's BYTES are not, and stream from the human write surface's
	// own route beside it (internal/api/workapi's files.go), which the
	// literal segment keeps apart from /worklet/{id}.
	{method: "GET", pattern: "/worklet/files", what: "work_files"},
	{method: "GET", pattern: "/worklet/views/saved", what: "work_saved_views"},
	{method: "GET", pattern: "/worklet/catalogue", what: "work_catalogue"},
	{method: "GET", pattern: "/worklet/people/{handle}", what: "work_person", path: map[string]string{"handle": "handle"}},
	{method: "GET", pattern: "/worklet/{id}", what: "work_item", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/worklet", what: "work_items"},
	// The page ACTIVITY and one REVISION's body. Both above /doclet/pages/{id},
	// for the reason the work routes give: a literal segment beats the
	// wildcard, so neither is read as a page whose id is "activity".
	{method: "GET", pattern: "/doclet/pages/activity", what: "page_activity"},
	{method: "GET", pattern: "/doclet/pages/{id}/revisions/{version}", what: "page_revision", path: map[string]string{"id": "page", "version": "version"}},
	{method: "GET", pattern: "/doclet/pages/{id}", what: "page", path: map[string]string{"id": "id"}},
	{method: "GET", pattern: "/doclet/pages", what: "pages"},
	{method: "GET", pattern: "/containers", what: "containers"},
	// WHAT THE COMPANY DID, merged across Worklet, Doclet and the schedules,
	// and not under /worklet/, since two of its three sources are not
	// Worklet's.
	{method: "GET", pattern: "/feed", what: "company_feed"},
	// THE COMPANY'S OWN CHAT. Reads only — a write is not a question and
	// the registry answers questions: every ordinary chat write is a tool
	// the dashboard calls through /operator/act (ADR-0024), and the three
	// that destroy or set a room's horizon are the human write surface's
	// (internal/api/workapi/chat.go).
	//
	// The path values are named `channel_id` and `root_id`, which are the
	// names the QUESTIONS take — and those are the domain's own, because
	// [chatlet.Reader]'s refusals name the field they refused and those
	// sentences reach the caller unchanged. A shorter spelling here would
	// send somebody looking for a key this surface does not accept.
	{method: "GET", pattern: "/chatlet/channels", what: "chat_channels"},
	// THE LITERAL SEGMENTS BEAT THE WILDCARD, as they do under /worklet/ and
	// /doclet/pages/: /chatlet/channels/{channel_id}/messages is not read as a room
	// whose id is "messages", and net/http resolves the more specific
	// pattern rather than the first registered.
	{method: "GET", pattern: "/chatlet/channels/{channel_id}/messages", what: "chat_messages",
		path: map[string]string{"channel_id": "channel_id"}},
	// ONE THREAD, addressed by the message it hangs off. A thread here is
	// one level deep by construction, so this is a range in one room
	// rather than a walk — and it is a route of its own rather than a
	// filter on the transcript because the two answers have different
	// shapes: this one carries the root and who has spoken in it.
	{method: "GET", pattern: "/chatlet/channels/{channel_id}/threads/{root_id}", what: "chat_thread",
		path: map[string]string{"channel_id": "channel_id", "root_id": "root_id"}},
	{method: "GET", pattern: "/chatlet/channels/{channel_id}", what: "chat_channel",
		path: map[string]string{"channel_id": "channel_id"}},
	// WHAT NAMED ME. About the caller rather than about a room, which is
	// why it is not under /chatlet/channels/ — and resolved to a seat by the
	// server, because a caller may never name one.
	//
	// THERE IS NO `GET /chatlet/read` beside it. Where somebody has read to
	// is not a question of its own: it rides the rail, whose every room
	// carries this viewer's unread count and mute, because a badge and
	// the room it belongs to are read together or not at all. The WRITE
	// half is the `mark_chat_read` tool, which the dashboard calls through
	// /operator/act like every other write.
	{method: "GET", pattern: "/chatlet/mentions", what: "chat_mentions"},
	// THE READER'S OWN VIEWS, each about the caller rather than a room
	// (Activity, the threads they follow, what they saved and what they
	// sent), and a room's shared links and one of its messages, which are
	// under the room they read.
	{method: "GET", pattern: "/chatlet/activity", what: "chat_activity"},
	{method: "GET", pattern: "/chatlet/threads", what: "chat_threads"},
	{method: "GET", pattern: "/chatlet/saved", what: "chat_saved"},
	{method: "GET", pattern: "/chatlet/sent", what: "chat_sent"},
	{method: "GET", pattern: "/chatlet/channels/{channel_id}/links", what: "chat_links",
		path: map[string]string{"channel_id": "channel_id"}},
	{method: "GET", pattern: "/chatlet/channels/{channel_id}/messages/{message_id}",
		what: "chat_message",
		path: map[string]string{"channel_id": "channel_id", "message_id": "message_id"}},
	// RANKED, over every room the caller may read. Its own route beside
	// the transcript for the reason the board's search is one beside the
	// board: a transcript is a position in one room and keeps that room's
	// order, while a search ranks a corpus, so its answer IS the order.
	{method: "GET", pattern: "/chatlet/search", what: "chat_search"},
	// THE NAME OF EVERY ROOM THE CALLER MAY READ, joined or not: what prose
	// names a room by, which a reader cannot tell from a colour or an
	// issue's number without knowing the rooms.
	{method: "GET", pattern: "/chatlet/room-names", what: "chat_room_names"},
	// EVERY ROOM THE CALLER MAY BROWSE, with what a person chooses one by:
	// its member count, whether they are in it and when it was last active.
	{method: "GET", pattern: "/chatlet/rooms", what: "chat_rooms"},
	// WHO IS ASKING. Not under /worklet/: the answer is the caller's own
	// identity rather than anything the tracker holds, and a node with no
	// Worklet still has a viewer.
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
	for _, route := range a.overviewRoutes() {
		mux.Handle(route.pattern, a.serveFrom(route.kind, route.build))
	}
}

// overviewRoute is one of the reads served from the stream service.
type overviewRoute struct {
	pattern string
	kind    stream.Kind
	build   func(stream.Audience) any
}

// overviewRoutes are the four reads served from the stream service rather than
// the registry, in mount order.
func (a *App) overviewRoutes() []overviewRoute {
	return []overviewRoute{
		{"GET /agents", stream.KindAgents, func(stream.Audience) any { return a.stream.Roster() }},
		{"GET /org", stream.KindOrg, func(stream.Audience) any { return a.stream.Org() }},
		{"GET /tools", stream.KindTools, func(stream.Audience) any { return a.stream.Tools() }},
		{"GET /stream/snapshot", stream.KindSnapshot,
			func(audience stream.Audience) any { return a.stream.Snapshot(audience) }},
	}
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
//
// A PRINCIPAL ACTING THROUGH THE ASSISTANT reads them as its person does
// (ADR-0052's parity): each is the overview its person's socket is pushed,
// shaped for the same audience, and none carries a credential.
func (a *App) serveFrom(kind stream.Kind, build func(stream.Audience) any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.Caller(w, r)
		if !ok {
			return
		}
		out, err := a.overview(r.Context(), principal, kind, build)
		var refusal *queries.Refusal
		if errors.As(err, &refusal) {
			// THE ENVELOPE every refusal on authority is answered in,
			// rather than a sentence naming the permission: a client
			// reading `grants` off a refused question reads it off this
			// too, worded by the table's one mapping.
			authz.EnvelopeRefusal(w, r, authz.Policy{}, refusal.Decision)
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
		writeQueryError(w, r, what, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}
