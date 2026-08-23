// Package auth is the API's bearer-token guard.
//
// Tier A lists the accepted tokens under api.auth.tokens. Each entry has an id,
// recorded as the author of any write made with it, and a token resolved from
// the environment at startup. The middleware wraps the whole mux: it extracts
// the bearer token, compares it in constant time, and either attaches the
// operator id to the request context or answers 401.
//
// WHAT IT GUARDS IS A POLICY DECISION, NOT A FIXED PREFIX. Writes and the whole
// /config surface always need a token. Reads follow allow_anonymous_read, which
// defaults to open — [Guard.Requires] is the one place that rule is written
// down, and both the HTTP middleware and the WebSocket handshake consult it, so
// the two cannot end up guarded in one place and open in the other.
//
// The guard is mounted UNCONDITIONALLY. In the Python this replaces the
// middleware was mounted only when Tier A was present, while the /config write
// surface was gated on a store being configured — two independent conditions
// deciding one security property, coinciding only because every real caller
// happened to supply both. Tier A supplies the POSTURE, never the existence of
// a check: with no tokens at all, no candidate can match, so reads serve and
// every write and all of /config is refused.
package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("api.auth")

// GuardedPrefix is the config surface. Always guarded, and never eligible for
// allow_anonymous_read — reading it exposes the whole company document, and
// writing it changes the company.
const GuardedPrefix = "/config"

// AnonymousOperator is the attribution recorded when auth is disabled.
//
// Config refuses it as a token id, so a real operator's writes can never be
// confused in an audit row with the ones made while the guard was off. Taken
// from there rather than restated: two copies would disagree silently, each
// side staying self-consistent while the reservation stopped covering what the
// API actually stamps.
const AnonymousOperator = config.ReservedOperatorID

// unguardedExact and unguardedPrefixes are the routes served without a bearer
// token, because they authenticate by other means or because a client must
// reach them to obtain a token:
//
//   - /health, /ready: probes. An orchestrator has no token, and a liveness
//     check that 401s is a liveness check that fails.
//   - /webhooks/: every one verifies a provider signature before doing
//     anything, which is a stronger check than a shared bearer token. Includes
//     the Slack OAuth landing page, which a browser reaches mid-install with no
//     token in hand.
//   - /otlp/: the per-run signed token in the path IS the credential.
//   - the dashboard shell and its assets: the page that prompts for the token
//     cannot itself require one. It ships no data — every byte it renders comes
//     from an authenticated fetch.
//
// The split is deliberate. A PREFIX exempts everything beneath it, so only the
// three that genuinely have sub-paths get one, and each ends in a slash, which
// is what stops it exempting a sibling. /health and /ready are single
// endpoints, so they are exact: as prefixes they would silently have exempted
// any future route merely starting with those letters — a /health-admin, a
// /readyz-reset — on the day it was added.
var unguardedExact = map[string]struct{}{
	"/": {}, "/dashboard": {}, "/favicon.ico": {}, "/health": {}, "/ready": {},
}

var unguardedPrefixes = []string{"/webhooks/", "/otlp/", "/static/"}

// readMethods are treated as reads for allow_anonymous_read.
var readMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodOptions: {},
}

// loopbackHosts are bind addresses no other machine can reach. Anonymous reads
// on one of these are a laptop; anonymous reads on anything else are a decision
// somebody may not have made deliberately — which is the difference between
// stating the posture and warning about it.
var loopbackHosts = map[string]struct{}{
	"127.0.0.1": {}, "::1": {}, "localhost": {}, "localhost6": {},
}

// BindIsLoopback reports whether api.host binds an address only this machine
// can reach.
func BindIsLoopback(host string) bool {
	h := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	_, ok := loopbackHosts[h]
	return ok
}

// Unguarded reports whether a path is served without a bearer token.
//
// A single trailing slash is normalised away before the exact set is consulted,
// because the guard runs BEFORE routing: the mux would redirect /health/ to
// /health, but only if the request survives long enough to be routed. Without
// this, a load balancer configured to probe /health/ gets a 401 in the closed
// posture and takes the node out of rotation — an outage caused by a slash.
//
// Only the exact set is normalised, and only by one slash. The /config guard
// reads the raw path (/config/ starts with /config either way) and the prefix
// exemptions already end in a slash, so nothing here can widen what is exempt
// beyond the trailing-slash spelling of a path that was exempt already.
func Unguarded(path string) bool {
	if _, ok := unguardedExact[path]; ok {
		return true
	}
	for _, prefix := range unguardedPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		_, ok := unguardedExact[path[:len(path)-1]]
		return ok
	}
	return false
}

// Guard holds the loaded posture and answers every auth question about it.
type Guard struct {
	// tokens maps operator id to token. Empty is a real posture: no
	// candidate can match, so writes and /config are refused outright.
	tokens map[string]string

	disabled      bool
	anonymousRead bool
}

// New builds the guard from Tier A.
//
// It does not fail. The pairings that would leave nothing reachable — no tokens
// with anonymous reads turned off — and the reserved token id are refused by
// config validation, which is where a `crewlet validate` on a laptop can catch
// them rather than a process discovering them at bind time.
func New(b *config.Bootstrap) *Guard {
	if b == nil {
		// No Tier A at all: the posture is unset, so nothing can
		// authenticate. Reads serve, writes are refused — the same
		// answer as a config that lists no tokens, which is the honest
		// reading of "nobody has said who may write".
		return &Guard{anonymousRead: true}
	}
	auth := b.API.Auth
	if auth.Disabled {
		log.Warn("api_auth_disabled",
			"hint", "api.auth.disabled is true — every route, including LLM "+
				"transcripts on /events and /agents/{id}/memory, serves without "+
				"authentication. Never use in production.")
		return &Guard{disabled: true, anonymousRead: auth.AllowAnonymousRead}
	}

	tokens := make(map[string]string, len(auth.Tokens))
	for _, entry := range auth.Tokens {
		tokens[entry.ID] = entry.Token
	}
	if len(tokens) > 0 {
		log.Info("api_auth_tokens_loaded", "count", len(tokens))
	}
	return &Guard{tokens: tokens, anonymousRead: auth.AllowAnonymousRead}
}

// AnonymousRead reports the read posture, for the startup line that states it.
func (g *Guard) AnonymousRead() bool { return g.anonymousRead }

// Disabled reports whether the guard is off entirely.
func (g *Guard) Disabled() bool { return g.disabled }

// Tokens reports how many credentials are loaded, for the same startup line.
func (g *Guard) Tokens() int { return len(g.tokens) }

// Operator returns the operator id a bare token authenticates as.
//
// THE TOKEN COMPARISON, IN ONE PLACE. The HTTP middleware reaches it through
// [Guard.Bearer], which peels the Authorization header first, and the
// dashboard's WebSocket query channel calls it directly, because an
// operator-only query arrives as a field on a socket frame rather than as a
// header. Both therefore accept exactly the same tokens, honour disabled
// identically, and compare in constant time.
func (g *Guard) Operator(candidate string) (string, bool) {
	if g.disabled {
		// Every caller is accepted. The explicit label is what keeps a
		// disabled-mode write distinguishable in an audit row.
		return AnonymousOperator, true
	}
	// An empty candidate never authenticates, and the check is not
	// redundant with the compare below: config refuses an empty token
	// value, but Bootstrap is an exported struct an embedder can build
	// directly, and a token configured as "" would otherwise match a
	// request that presented no credential at all. A total bypass, from
	// one unset environment variable.
	if candidate == "" {
		return "", false
	}
	// Every token is compared, and the loop does not stop at the first
	// match: an early exit makes the time taken depend on WHICH id
	// matched, which is exactly the leak the constant-time compare below
	// exists to close.
	matched := ""
	for operatorID, expected := range g.tokens {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1 {
			matched = operatorID
		}
	}
	return matched, matched != ""
}

// Bearer returns the operator id for a request's Authorization header.
func (g *Guard) Bearer(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		// Not a bearer header at all. Still asked, because a disabled
		// guard accepts a request that carries no credential.
		return g.Operator("")
	}
	return g.Operator(strings.TrimSpace(header[len(scheme):]))
}

// Requires reports whether this request must carry a valid bearer token.
//
// The whole rule, in one function.
//
// allow_anonymous_read, on by default, opens READS only. What that opens is
// worth naming rather than leaving to the reader's imagination: /events,
// /agents/{id}/memory and /ws/stream carry full LLM transcripts — prompts, tool
// arguments, diary entries. Turning it off closes them, and the dashboard then
// authenticates its socket like any other client.
func (g *Guard) Requires(path, method string) bool {
	if Unguarded(path) {
		return false
	}
	if strings.HasPrefix(path, GuardedPrefix) {
		return true
	}
	if g.anonymousRead {
		_, isRead := readMethods[strings.ToUpper(method)]
		return !isRead
	}
	return true
}

// operatorKey carries the authenticated operator id down the handler chain.
type operatorKey struct{}

// OperatorFrom returns the operator id the guard attached, if any.
func OperatorFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(operatorKey{}).(string)
	return id, ok
}

// WithOperator attaches an operator id, for a surface that authenticates
// outside the middleware — the WebSocket handshake, whose credential arrives on
// the query string rather than as a header.
func WithOperator(ctx context.Context, operatorID string) context.Context {
	return context.WithValue(ctx, operatorKey{}, operatorID)
}

// Middleware wraps a handler with the guard.
//
// HTTP only. A WebSocket upgrade is an HTTP request and does pass through here,
// but the dashboard's socket carries its credential as a query parameter rather
// than a header, so the stream handler authenticates it itself — and closes
// with 1008 BEFORE accepting, which is the one thing a browser client can
// actually observe.
func (g *Guard) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if !g.Requires(path, r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		operatorID, ok := g.Bearer(r)
		if !ok {
			log.Warn("api_auth_failed",
				"route", path,
				"reason", "missing_or_invalid_bearer",
				// The candidate value is NEVER logged: a rejected token
				// is still a credential, and a log is a place it would
				// outlive the request.
				"remote", remoteHost(r))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
			return
		}
		log.Debug("api_auth_ok", "operator_id", operatorID, "route", path)
		next.ServeHTTP(w, r.WithContext(WithOperator(r.Context(), operatorID)))
	})
}

// remoteHost is the caller's address without its port, for the failure log.
func remoteHost(r *http.Request) string {
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}
