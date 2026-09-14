package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
)

// The drain gate.
//
// A draining node KEEPS ITS LISTENER, because the drain is exactly when an
// orchestrator is watching it: /health has to stay 200 so the node is not
// killed in the middle of the turns the drain exists to finish, and /ready has
// to answer 503 so traffic moves elsewhere. A node that closed its listener
// before draining answered neither, and a liveness probe that cannot connect
// is a liveness probe that fails.
//
// What a drain must not do is keep taking work. It waits for the turns already
// running, and a surface still admitting deliveries would keep making more. So
// the door is this gate rather than the listener: from the first moment of a
// drain every request that would start work is refused with a 503 naming the
// drain, and everything else is served until the drain completes and the
// process closes the listener.

// DrainRetryAfter is what a refusal tells the caller to wait.
//
// THIRTY SECONDS, from what takes a draining node out of rotation. /ready
// answers 503 from the drain's first moment, but a load balancer acts on a
// probe only after its own period and failure threshold, which on Kubernetes'
// defaults is three failures ten seconds apart. A retry sooner than that
// mostly reaches this same node again; one after it reaches a peer, or on a
// single node the process that restarted.
const DrainRetryAfter = 30 * time.Second

// servedWhileDraining reports whether a request is still served once this node
// has begun to drain.
//
// The whole rule, in one function:
//
//   - The webhook edge is REFUSED, whatever the method. A delivery is new work
//     by definition, and the edge's two GET routes are landings that act: the
//     GitHub App return converts a creation code into a sealed credential and a
//     config revision, and an install arrival asks the reconcile loop for a
//     pass.
//   - The sandbox bridge and the telemetry edge are SERVED. They carry the tool
//     calls and the spans of coding runs that started before the drain, which
//     is the work the drain is waiting for, and refusing them would strand a
//     run mid-flight on the node that is waiting for it to finish.
//   - Every other read is SERVED: the probes, the dashboard, the REST reads and
//     the live socket. A read starts nothing, and it is how an operator watches
//     the drain.
//   - Every other write is REFUSED. A config write activates a revision, a
//     setup write runs a vendor's pass, a backup copies the estates the
//     teardown is about to close, and the operator MCP files and moves work.
//     Refusing by default is what keeps a write route added later from being
//     admitted through a drain because nobody listed it.
func servedWhileDraining(r *http.Request) bool {
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, auth.WebhookPrefix):
		return false
	case strings.HasPrefix(path, mcpbridge.PathPrefix), strings.HasPrefix(path, auth.OTLPPrefix):
		return true
	default:
		return auth.IsRead(r.Method)
	}
}

// drainGate refuses new work once this node has begun to drain.
//
// Inside the auth guard, so a credential is still the first question a guarded
// route asks, and inside the browser posture, so a cross-origin dashboard can
// read the refusal rather than seeing an opaque network error.
func (a *App) drainGate(next http.Handler) http.Handler {
	retryAfter := strconv.Itoa(int(DrainRetryAfter / time.Second))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if servedWhileDraining(r) || !a.draining() {
			next.ServeHTTP(w, r)
			return
		}
		// INFO, not a warning: this is the drain doing its job. The path
		// only, never the query, which is where a Confluence Cloud
		// delivery carries its shared token.
		log.Info("api_refused_while_draining", "method", r.Method, "route", r.URL.Path)
		w.Header().Set("Retry-After", retryAfter)
		httpjson.FailWith(w, http.StatusServiceUnavailable, httpjson.CodeDraining,
			map[string]string{
				"detail": "this node is draining for a shutdown: the turns already " +
					"running finish, and nothing new is started here",
				"hint": "retry against another node, or once this one has restarted; " +
					"/ready answers 503 for as long as the drain lasts",
			})
	})
}

// draining reports whether this node has begun to drain. A process with no
// engine has nothing to drain, so it never is.
func (a *App) draining() bool { return a.runtime != nil && a.runtime.ShuttingDown() }
