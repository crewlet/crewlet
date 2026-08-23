package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
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

	// Query answers the socket's request/response channel. Nil answers
	// every query as unknown, which is what a process with no query
	// surface honestly is.
	Query stream.Query

	// QueueBackend names the broker, for the health body.
	QueueBackend string

	// Now is injectable so a test can pin the timestamps.
	Now func() time.Time

	// HealthInterval overrides the shared tick's cadence.
	HealthInterval time.Duration
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
		Health:         a.streamHealth,
		Now:            now,
		HealthInterval: opts.HealthInterval,
	})

	mux := http.NewServeMux()
	mux.Handle("GET /health", http.HandlerFunc(a.serveHealth))
	mux.Handle("GET /ready", http.HandlerFunc(a.serveReady))
	mux.Handle("/ws/stream", stream.Handler(a.guard, a.stream, opts.Query))
	a.handler = a.guard.Middleware(mux)
	return a
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
