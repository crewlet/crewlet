package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/static"
)

// App is the HTTP surface: the dashboard, the live socket and the REST routes.
type App struct {
	guard  *auth.Guard
	state  *livestate.LiveState
	stream *stream.Service

	// runtime is nil on a standalone API. See NodeRuntime.
	runtime NodeRuntime

	nodeID       string
	startedAt    string
	queueBackend string

	// queries is the read surface both transports answer from.
	queries *queries.Registry

	// configured flips once a company revision is active. Atomic because
	// the config refresher sets it from its own goroutine while every
	// health probe reads it.
	configured atomic.Bool

	handler http.Handler
}

// Options configure the app.
type Options struct {
	// Bootstrap supplies the auth posture and the node's identity. Nil is
	// permitted and is not the same as absent config: the guard then
	// refuses every write, because nobody has said who may make one.
	Bootstrap *config.Bootstrap

	// Runtime is the co-located engine, or nil for a standalone API.
	Runtime NodeRuntime

	// State is the projection to serve. Nil builds an empty one.
	State *livestate.LiveState

	// Sources are what the read surface answers from. A source left nil
	// makes its questions UNREGISTERED rather than failing — the honest
	// answer for a node that does not have that surface at all, and
	// distinct from an empty one.
	Sources queries.Sources

	// QueueBackend names the broker, for the health body.
	QueueBackend string

	// Now is injectable so a test can pin the timestamps.
	Now func() time.Time

	// HealthInterval overrides the shared tick's cadence.
	HealthInterval time.Duration

	// Inbound wires the webhook edge. Zero means this process serves no
	// webhook endpoint — see [Inbound].
	Inbound Inbound

	// Config serves /config. Nil serves none, which is what a process with
	// no store genuinely has.
	Config *configapi.Service

	// Assets overrides the embedded dashboard tree. Nil serves the one
	// compiled into the binary, which is what every deployment does; a
	// test supplies its own to assert about serving rather than about the
	// dashboard's current contents.
	Assets fs.FS
}

// New assembles the app.
//
// The auth guard is mounted UNCONDITIONALLY. Tier A supplies the posture, never
// the existence of a check — see the auth package for what the alternative cost
// the Python this replaces.
func New(opts Options) *App {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	state := opts.State
	if state == nil {
		state = livestate.New()
	}

	a := &App{
		guard:        auth.New(opts.Bootstrap),
		state:        state,
		runtime:      opts.Runtime,
		nodeID:       nodeIDOf(opts.Bootstrap),
		startedAt:    nowISO(now()),
		queueBackend: opts.QueueBackend,
	}
	a.stream = stream.NewService(state, stream.Options{
		Health: a.streamHealth,
		// Read through the SOURCES rather than captured, for the same
		// reason every other read here is: a config apply replaces the
		// company, and a map captured at boot would keep cross-linking a
		// renamed seat to the handle it used to have.
		Handles:        opts.Sources.RoleHandles,
		Now:            now,
		HealthInterval: opts.HealthInterval,
	})

	tree := opts.Assets
	if tree == nil {
		tree = static.FS()
	}
	files := newAssets(tree)

	// ONE registry, and both transports go through it. Two surfaces
	// answering one question from two implementations is how they end up
	// disagreeing with nobody noticing.
	sources := opts.Sources
	if sources.Health == nil {
		sources.Health = func() any { return a.health() }
	}
	if sources.State == nil {
		sources.State = state
	}
	a.queries = queries.NewRegistry()
	queries.Register(a.queries, sources)

	mux := http.NewServeMux()
	mux.Handle("GET /health", http.HandlerFunc(a.serveHealth))
	mux.Handle("GET /ready", http.HandlerFunc(a.serveReady))
	mux.Handle("GET /query/{what}", http.HandlerFunc(a.serveQuery))
	mux.Handle("/ws/stream", stream.Handler(a.guard, a.stream, a.answer))
	// The dashboard shell and its assets. All four paths are exempt from
	// the guard: the page that prompts for a token cannot itself require
	// one, and it ships no data — every byte it renders comes from an
	// authenticated fetch.
	mux.Handle("GET /{$}", http.RedirectHandler("/dashboard", http.StatusFound))
	mux.Handle("GET /dashboard", http.HandlerFunc(files.serveIndex))
	mux.Handle("GET /favicon.ico", http.HandlerFunc(files.serveFavicon))
	mux.Handle("GET /static/", http.HandlerFunc(files.serveStatic))
	// The inbound edge. Exempt from the guard by prefix (see the auth
	// package) because each route authenticates by provider credential,
	// which is why every one of them verifies before it does anything.
	a.mountWebhooks(mux, opts.Inbound, sources, now)
	// The config surface. GUARDED in full, reads included: the auth
	// package makes /config the one prefix never eligible for
	// allow_anonymous_read, because reading it exposes the whole company
	// document and writing it changes the company.
	opts.Config.Routes(mux)
	a.handler = a.guard.Middleware(mux)
	return a
}

// Inbound is what the webhook edge needs that only the surrounding process
// has: somewhere to republish a delivery, the epoch's verification material,
// and the cross-process dedupe.
//
// The rest of what the edge needs — the event log, the live stream, whether a
// revision is active, the clock — comes from the app itself, so those cannot be
// wired differently here than they are everywhere else on this node.
//
// A nil Publisher turns the edge OFF, and it is the one field that decides it:
// the edge exists exactly when there is a queue to republish onto, because
// recording a delivery that never reaches an agent is worse than not accepting
// it.
type Inbound struct {
	Secrets    func() webhooks.Secrets
	Publisher  queue.Publisher
	Deliveries *store.Deliveries

	// Keys verifies Forge invocation tokens. Nil uses Atlassian's
	// published JWKS.
	Keys webhooks.KeySource
}

// mountWebhooks registers the inbound edge, or says why it did not.
//
// Silence is the failure mode here: an operator whose integration never fires
// has no way to tell a misconfigured provider from a node that never had the
// endpoint, and the webhook is the only surface where "nothing happened" is
// the normal appearance of both.
func (a *App) mountWebhooks(mux *http.ServeMux, in Inbound, sources queries.Sources, now func() time.Time) {
	if in.Publisher == nil {
		log.Warn("webhooks_disabled",
			"hint", "this process has no event queue, so it serves no webhook "+
				"endpoint and every integration pointed at it will 404")
		return
	}
	webhooks.New(webhooks.Options{
		Secrets:    in.Secrets,
		Publisher:  in.Publisher,
		Deliveries: in.Deliveries,
		Keys:       in.Keys,
		Events:     sources.Events,
		Stream:     a.stream,
		Configured: a.Configured,
		Now:        now,
	}).Routes(mux)
}

func nodeIDOf(b *config.Bootstrap) string {
	if b == nil || b.Node.ID == "" {
		return config.DefaultNodeID
	}
	return b.Node.ID
}

// ServeHTTP makes the app the process's handler.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.handler.ServeHTTP(w, r) }

// Stream exposes the live channel, for the engine to feed.
func (a *App) Stream() *stream.Service { return a.stream }

// State exposes the projection.
func (a *App) State() *livestate.LiveState { return a.state }

// Guard exposes the auth posture, for the startup line that states it.
func (a *App) Guard() *auth.Guard { return a.guard }

// Configured reports whether a company revision is active.
func (a *App) Configured() bool { return a.configured.Load() }

// SetConfigured records that a revision applied, or stopped being active.
//
// Load-bearing on readiness: an unconfigured node cannot verify a webhook
// signature, so it must leave rotation rather than answer deliveries it would
// only reject.
func (a *App) SetConfigured(v bool) { a.configured.Store(v) }

// Start brings up the shared health tick.
func (a *App) Start(ctx context.Context) { a.stream.StartHealthTicks(ctx) }

// Stop ends the tick and disconnects every client.
func (a *App) Stop() { a.stream.Stop() }

func (a *App) serveHealth(w http.ResponseWriter, _ *http.Request) {
	// ALWAYS 200 while the process is alive, INCLUDING through a drain: an
	// orchestrator watching liveness must not SIGKILL a node that is
	// finishing its in-flight turns. /ready is what steers traffic.
	writeJSON(w, http.StatusOK, a.health())
}

func (a *App) serveReady(w http.ResponseWriter, _ *http.Request) {
	body, status := a.readiness()
	writeJSON(w, status, body)
}

// writeJSON is the one response writer, so every route spells a body the same
// way — including the status, which has to be written before the body or the
// header is already gone.
func writeJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		log.Error("api_encode_failed", "error", err)
		http.Error(w, `{"error":"encode_failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// Queries exposes the read surface, for a caller that wants to know what this
// node can answer.
func (a *App) Queries() *queries.Registry { return a.queries }

// answer bridges the registry to the socket's error codes.
//
// The codes are the wire protocol's and the errors are the surface's, so the
// mapping lives at exactly one boundary — here. A query surface that returned
// wire codes would be a domain package encoding a transport's vocabulary, and
// a transport that classified errors itself would be a second place for the
// two to disagree about what "unauthorized" means.
func (a *App) answer(ctx context.Context, what string, params map[string]any, operatorID string) (any, error) {
	data, err := a.queries.Answer(ctx, what, params, operatorID)
	switch {
	case err == nil:
		return data, nil
	case errors.Is(err, queries.ErrUnknown):
		return nil, fmt.Errorf("%w: %s", stream.ErrUnknownQuery, what)
	case errors.Is(err, queries.ErrUnauthorized):
		return nil, fmt.Errorf("%w: %s", stream.ErrUnauthorized, what)
	default:
		return nil, err
	}
}

// serveQuery answers a question over HTTP.
//
// The SAME registry the socket uses, reached with the same name — so a
// dashboard in degraded mode (no socket) and one with a socket are looking at
// one implementation, not two that agree today.
func (a *App) serveQuery(w http.ResponseWriter, r *http.Request) {
	what := r.PathValue("what")
	operatorID, _ := auth.OperatorFrom(r.Context())

	// Params come from the query string, read through the same accessors a
	// socket frame's JSON object goes through — which is what stops a
	// filter being honoured on one transport and ignored on the other.
	data, err := a.queries.AnswerWith(r.Context(), what,
		queries.FromQuery(r.URL.Query()), operatorID)
	if err == nil {
		writeJSON(w, http.StatusOK, data)
		return
	}
	switch {
	case errors.Is(err, queries.ErrUnknown):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": stream.CodeUnknownQuery})
	case errors.Is(err, queries.ErrUnauthorized):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": stream.CodeUnauthorized})
	case errors.Is(err, queries.ErrBadParams):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": stream.CodeQueryFailed})
	default:
		// The reason reaches the LOG, not the caller: it can carry a
		// database path or a driver's own message, and this route is
		// reachable under the anonymous read posture.
		log.Warn("api_query_failed", "what", what, "error", err)
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": stream.CodeQueryFailed})
	}
}
