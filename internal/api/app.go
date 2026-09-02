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
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
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

	// budgets is the fleet's token counter, for the one route that WRITES
	// to it. Nil on a standalone API with no coordination store, which
	// that route reports rather than hides.
	budgets budgetResetter

	// backup takes a copy of this node's durable state. Nil on a process
	// holding neither a store nor a broker, which that route reports
	// rather than hides.
	backup backupTaker

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

	// Secrets serves /secrets — the fleet's credential store. Nil serves
	// none, which is what a process that cannot reach the coordination
	// store genuinely has; the routes then 404 rather than 500.
	Secrets *secretsapi.Service

	// OtelReceiver serves the sandbox telemetry edge. Nil serves none, and
	// the route is then ABSENT rather than refusing — an endpoint that
	// exists and answers 503 to everything reads as broken, while one that
	// is not there matches what the config says.
	//
	// It belongs to the API rather than to the engine because in a SPLIT
	// deployment this is the externally reachable process: the engine
	// mints a run's endpoint, and a different process verifies the token.
	// That is why the token is signed rather than stored.
	OtelReceiver *sandbox.OtelReceiver

	// Bridge serves a running seat's tool surface to a coding agent over
	// MCP. Nil serves none, and the route is then ABSENT for the same
	// reason OtelReceiver's is.
	//
	// It belongs to the API for the same reason too: in a SPLIT deployment
	// this is the externally reachable process, so the engine opens a run's
	// session and a different process verifies its token. That is why the
	// token is signed rather than stored — see internal/runtoken.
	Bridge *mcpbridge.Bridge

	// Budgets is the fleet's token counter. Supplied separately from
	// Sources.Budget, which is the READ half: a reset is an operator
	// action against a spend ceiling, and giving the read surface a
	// method that clears one would put it a typo away from every screen
	// that renders spend.
	Budgets budgetResetter

	// Backup copies this node's durable state to a path an operator
	// names. Nil where there is nothing to copy — a process running
	// neither a store nor a broker — which the route reports as such.
	Backup backupTaker

	// Assets overrides the embedded dashboard tree. Nil serves the one
	// compiled into the binary, which is what every deployment does; a
	// test supplies its own to assert about serving rather than about the
	// dashboard's current contents.
	Assets fs.FS
}

// New assembles the app.
//
// The auth guard is mounted UNCONDITIONALLY. Tier A supplies the posture, never
// the existence of a check — see the auth package for what the alternative
// costs.
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
		Handles: opts.Sources.RoleHandles,
		// The three config-derived surfaces, read live for the same
		// reason Handles is: an apply replaces the company.
		Roster: func() []map[string]any { return roster(opts.Sources.Company, opts.Runtime) },
		Org:    func() map[string]any { return orgTree(opts.Sources.Company) },
		Tools:  func() []map[string]any { return toolRows(opts.Runtime) },
		// The CONFIGURED rows only. The dispatch ledger is a store read
		// and the snapshot makes none; the screen fetches that half
		// itself through the `schedules` question.
		Schedules: func() any { return opts.Sources.ConfiguredSchedules() },

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
	// Only a co-located engine knows which parsers registered. Left nil on
	// a standalone API, which is the honest answer rather than a confident
	// empty list.
	if sources.Routed == nil && opts.Runtime != nil {
		sources.Routed = func() []string { return opts.Runtime.Snapshot().RoutedSources }
	}
	// And only a co-located engine knows what its ${VAR}s resolved to.
	if sources.Verifiable == nil && opts.Runtime != nil {
		sources.Verifiable = func() []string {
			return opts.Runtime.Snapshot().VerifiableSources
		}
	}
	a.queries = queries.NewRegistry()
	queries.Register(a.queries, sources)
	a.budgets = opts.Budgets
	a.backup = opts.Backup

	mux := http.NewServeMux()
	mux.Handle("GET /health", http.HandlerFunc(a.serveHealth))
	mux.Handle("GET /ready", http.HandlerFunc(a.serveReady))
	mux.Handle("GET /query/{what}", http.HandlerFunc(a.serveQuery))
	// The NAMED read routes — the public REST API. Adapters over the same
	// registry the generic form above reaches; see rest.go.
	a.mountReads(mux)
	// The one WRITE outside /config and the webhook edge. A POST, so the
	// anonymous-read posture never opens it: clearing a company's spend
	// ceiling is not a read, whatever a laptop deployment allows.
	mux.Handle("POST /budgets/reset", http.HandlerFunc(a.serveBudgetReset))
	// Also a POST, and for the same reason: copying every credential and
	// every seat's memory to a path the caller names is not a read,
	// whatever the anonymous-read posture allows.
	mux.Handle("POST /backup", http.HandlerFunc(a.serveBackup))
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
	// The SANDBOX TELEMETRY edge, exempt by the same prefix rule and for
	// the same reason: the exporter inside a box holds no API token, and
	// giving it one would hand a sandbox the credential that reads the
	// whole company. Its per-run token is in the path instead.
	a.mountOTLP(mux, opts.OtelReceiver)
	a.mountBridge(mux, opts.Bridge)
	// The config surface. GUARDED in full, reads included: the auth
	// package makes /config one of the two prefixes never eligible for
	// allow_anonymous_read, because reading it exposes the whole company
	// document and writing it changes the company.
	opts.Config.Routes(mux)
	// The other one. /secrets is how a rotation reaches a fleet at all —
	// the coordination broker is inside the engine's process on the
	// default topology, so no second process can write the store — and its
	// listing alone says which credentials a company holds.
	opts.Secrets.Routes(mux)
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
	Secrets   func() webhooks.Secrets
	Publisher queue.Publisher
	Claims    coord.Claims

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
		Claims:     in.Claims,
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
	if err != nil {
		writeQueryError(w, what, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// writeQueryError maps one failed question onto a status.
//
// SHARED by the generic /query/{what} form and every named route, because a
// caller must not learn that a seat does not exist from one path and that the
// server broke from the other.
func writeQueryError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, queries.ErrUnknown):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": stream.CodeUnknownQuery})
	case errors.Is(err, queries.ErrUnauthorized):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": stream.CodeUnauthorized})
	case errors.Is(err, queries.ErrBadParams):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": stream.CodeQueryFailed})
	default:
		// The reason reaches the LOG, not the caller: it can carry a
		// database path or a driver's own message, and these routes are
		// reachable under the anonymous read posture.
		log.Warn("api_query_failed", "what", what, "error", err)
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": stream.CodeQueryFailed})
	}
}
