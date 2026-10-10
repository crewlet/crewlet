// Package auth is the API's access guard: it decides WHO a request is, and
// refuses one that asks for more than its key reaches.
//
// # Who a request is
//
// Tier A lists the accepted keys under api.auth.tokens. Each has an id,
// recorded as the author of any write made with it, a ROLE — `member` or
// `admin` — and a value resolved from the environment at startup. The
// middleware wraps the whole mux, extracts the presented key, compares it in
// constant time and attaches a [Principal] to the request: the key's id, its
// role and the [Reach] that role carries. A request presenting no key is a
// principal too, an anonymous one, whose reach is what `api.auth.anonymous`
// opens. Every request has exactly one, so no handler has to tell "nobody
// attached anything" from "nobody".
//
// # What it may reach
//
// EVERY ROUTE NAMES ITS REACH where it is mounted ([Router]), and [Guard.Require]
// refuses a caller below it before the handler runs: 401 for a caller with no
// accepted key, 403 for one whose key is accepted and reaches less. Reach
// replaced a list of always-guarded prefixes beside a switch that opened every
// other read — transcripts, diaries and the event log among them — to anyone
// who could reach the port, and a "person" key that reached everything an
// operator key did. ADR-0031 is the decision; [Reach] states the one rule that
// draws the line between member and admin.
//
// The guard is mounted UNCONDITIONALLY. Tier A supplies the POSTURE, never the
// existence of a check: with no keys at all, no candidate can match, so every
// caller is anonymous and is served what the anonymous posture opens.
package auth

import (
	"context"
	"crypto/subtle"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
)

var log = logging.Get("api.auth")

// AnonymousOperator is the attribution recorded when auth is disabled.
//
// Config refuses it as a token id, so a real operator's writes can never be
// confused in an audit row with the ones made while the guard was off, and the
// chart refuses it as a seat binding, so a caller the guard never checked is
// never a person. Taken from [org.ReservedOperatorID] rather than restated: two
// copies would disagree silently, each side staying self-consistent while the
// reservation stopped covering what the API actually stamps.
const AnonymousOperator = org.ReservedOperatorID

// publicPrefixes are the routes OUTSIDE PARTIES call: a vendor delivering a
// webhook or returning a browser from its app flow, and a sandbox box exporting
// telemetry or calling its seat's tools. None of them holds an operator
// credential, each authenticates by its own (a provider signature, a per-run
// signed token), and they are the only routes a deployment has to publish
// beyond the people who run it.
//
// What the dedicated public listener (Tier A api.public) serves and api.port
// then refuses, decided by [Public] and nothing else. Every route under one of
// these is mounted at [ReachOpen] — the API's route gate holds the table to
// that — because the public listener is reached by callers that hold no key,
// and a route there needing one would answer every one of them 401.
//
// The dashboard's /static/ is open and NOT public: it is the shell of the
// dashboard, served where the dashboard is.
//
// UNEXPORTED, and read outside this package only through [PublicPrefixes]'
// copy, so no caller can append to the partition from outside it.
var publicPrefixes = []string{WebhookPrefix, OTLPPrefix, mcpbridge.PathPrefix}

// PublicPrefixes is a copy of the prefixes of the routes outside parties call.
// See [publicPrefixes].
func PublicPrefixes() []string { return slices.Clone(publicPrefixes) }

// Public reports whether a path is one of the routes outside parties call. See
// [publicPrefixes].
func Public(path string) bool {
	for _, prefix := range publicPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// WebhookPrefix and OTLPPrefix are the two exempt edges a second rule also
// reads. Named here, beside the exemption, for the reason [SocketPath] is: the
// API's drain gate refuses the first and serves the second, and a prefix it
// spelled for itself could stop matching the one this guard exempts without
// either rule looking wrong on its own.
const (
	WebhookPrefix = "/webhooks/"
	OTLPPrefix    = "/otlp/"
)

// readMethods are the methods that change nothing.
var readMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodOptions: {},
}

// IsRead reports whether a method is a read: GET, HEAD or OPTIONS.
//
// The drain gate serves reads because a read changes nothing and starts
// nothing, and a second list of which methods those are would be a second
// answer to one question.
func IsRead(method string) bool {
	_, ok := readMethods[strings.ToUpper(method)]
	return ok
}

// loopbackHosts are bind addresses no other machine can reach — which is the
// difference, on the startup line, between a laptop and a deployment.
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

// Guard holds the loaded posture and answers every access question about it.
type Guard struct {
	// keys maps a key's id to its value and role. Empty is a real posture:
	// no candidate can match, so every caller is anonymous.
	keys map[string]key

	// anonymous is the reach of a caller presenting no accepted key.
	anonymous Reach
	// posture is the Tier A value it came from, for the startup line and
	// the `access` answer.
	posture config.AnonymousAccess

	disabled bool
}

// key is one accepted credential.
type key struct {
	value string
	role  config.TokenRole
}

// New builds the guard from Tier A.
//
// It does not fail. The pairings that would leave nothing reachable, a key
// with no role and the reserved id are refused by config validation, which is
// where a `crewlet validate` on a laptop can catch them rather than a process
// discovering them at bind time.
func New(b *config.Bootstrap) *Guard {
	if b == nil {
		// No Tier A at all: nobody has said who may do anything, so no
		// key can match and a caller reaches only what is open to anyone.
		// Not the public posture — that is something Tier A opens, and
		// there is no Tier A to have opened it.
		return &Guard{anonymous: ReachOpen, posture: config.AnonymousNone}
	}
	auth := b.API.Auth
	if auth.Disabled {
		log.Warn("api_auth_disabled",
			"hint", "api.auth.disabled is true — every caller is an admin, and "+
				"every route, LLM transcripts and the secret store included, "+
				"serves without a key. Never use in production.")
		return &Guard{disabled: true, anonymous: ReachAdmin, posture: auth.Anonymous}
	}

	keys := make(map[string]key, len(auth.Tokens))
	for _, entry := range auth.Tokens {
		keys[entry.ID] = key{value: entry.Token, role: entry.Role}
	}
	if len(keys) > 0 {
		log.Info("api_auth_tokens_loaded", "count", len(keys))
	}
	return &Guard{keys: keys, anonymous: ReachOfAnonymous(auth.Anonymous), posture: auth.Anonymous}
}

// Anonymous is the posture a caller with no key is served under, for the
// startup line that states it and the `access` answer.
func (g *Guard) Anonymous() config.AnonymousAccess { return g.posture }

// Disabled reports whether the guard is off entirely.
func (g *Guard) Disabled() bool { return g.disabled }

// Tokens reports how many keys are loaded, for the same startup line.
func (g *Guard) Tokens() int { return len(g.keys) }

// TokenIDs names the keys this guard accepts, sorted — their LABELS and never
// their values, for the `access` answer.
//
// Read off the guard rather than off Tier A, because the guard is what decides:
// a disabled guard accepts every caller as [AnonymousOperator] and no listed
// key at all, and an answer built from the document would name keys that
// authenticate nobody.
func (g *Guard) TokenIDs() []string {
	return slices.Sorted(maps.Keys(g.keys))
}

// RoleOf is the role of the key with this id, or "" for none this guard
// accepts.
func (g *Guard) RoleOf(id string) config.TokenRole { return g.keys[id].role }

// AnonymousPrincipal is who a caller presenting no accepted key is.
func (g *Guard) AnonymousPrincipal() Principal {
	if g.disabled {
		// Every caller is accepted, as an admin: the explicit label is
		// what keeps a disabled-mode write distinguishable in an audit row.
		return Principal{ID: AnonymousOperator, Role: config.RoleAdmin, Reach: ReachAdmin}
	}
	return Principal{Reach: g.anonymous}
}

// Principal returns who a bare key authenticates as.
//
// THE KEY COMPARISON, IN ONE PLACE. The HTTP middleware and the socket
// handshake reach it through [Guard.Presented], which peels the key off the
// request first, and the dashboard's socket calls it directly for a key a
// frame presents for one question. All three therefore accept exactly the
// same keys, honour disabled identically, and compare in constant time.
func (g *Guard) Principal(candidate string) (Principal, bool) {
	if g.disabled {
		return g.AnonymousPrincipal(), true
	}
	// An empty candidate never authenticates, and the check is not
	// redundant with the compare below: config refuses an empty key
	// value, but Bootstrap is an exported struct an embedder can build
	// directly, and a key configured as "" would otherwise match a
	// request that presented no credential at all. A total bypass, from
	// one unset environment variable.
	if candidate == "" {
		return Principal{}, false
	}
	// Every key is compared, and the loop does not stop at the first
	// match: an early exit makes the time taken depend on WHICH id
	// matched, which is exactly the leak the constant-time compare below
	// exists to close.
	matched := ""
	for id, k := range g.keys {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(k.value)) == 1 {
			matched = id
		}
	}
	if matched == "" {
		return Principal{}, false
	}
	role := g.keys[matched].role
	return Principal{ID: matched, Role: role, Reach: ReachOfRole(role)}, true
}

// SocketPath is the dashboard's live socket, the one route whose credential
// may arrive on the query string. It is named here, beside the rule that
// reads it, so the mux that mounts it and the handler that serves it cannot
// spell it differently from the guard that admits it.
const SocketPath = "/ws/stream"

// Credential returns the key a request presented, or "".
//
// THE ONE PLACE A REQUEST'S CREDENTIAL IS READ. The Authorization bearer
// header on every route; and on the socket path only, the token query
// parameter as well, because a browser cannot set a header on a WebSocket
// constructor and the dashboard has no other way to send one. The query is
// read nowhere else: a key in a URL appears in proxy logs and browser
// history, which is a price worth paying for exactly one route that has no
// alternative and for no route that has.
//
// The header wins when both are present, so a non-browser client that sent
// the right header is never judged by a stale query.
func (g *Guard) Credential(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const scheme = "bearer "
	if len(header) >= len(scheme) && strings.EqualFold(header[:len(scheme)], scheme) {
		return strings.TrimSpace(header[len(scheme):])
	}
	if r.URL.Path == SocketPath {
		return r.URL.Query().Get("token")
	}
	return ""
}

// Presented returns who a request is, and whether it presented a key the guard
// rejected — the one case where an anonymous answer would be a lie about what
// the caller tried to do.
//
// Asked even when the request carries none, because a disabled guard accepts
// a request with no credential at all.
func (g *Guard) Presented(r *http.Request) (p Principal, rejected bool) {
	candidate := g.Credential(r)
	if p, ok := g.Principal(candidate); ok {
		return p, false
	}
	return g.AnonymousPrincipal(), candidate != ""
}

// rejectedKey marks a request that presented a key this guard did not accept.
type rejectedKey struct{}

// Middleware resolves who every request is and attaches it ([PrincipalFrom]).
//
// IT REFUSES NOTHING ON ITS OWN: what a route needs is the route's to say, at
// the mount ([Guard.Require]), so this cannot disagree with the table about
// which paths are guarded. A key the guard REJECTED is remembered beside the
// anonymous principal, so a route that needs a key answers that request 401
// for the key it sent rather than serving an open route as though none was —
// which it does, because an open route is open whatever was presented.
//
// A WebSocket upgrade is an HTTP request and passes through here like any
// other, which is why [Guard.Credential] reads the socket's query token.
func (g *Guard) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, rejected := g.Presented(r)
		ctx := WithPrincipal(r.Context(), p)
		if rejected {
			ctx = context.WithValue(ctx, rejectedKey{}, true)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Require serves next only to a caller whose reach covers need.
//
// THE ONE ENFORCEMENT POINT FOR ROUTES. Every route is mounted through it
// with the reach it declared, and the answer to a caller below that reach is
// one of two:
//
//   - 401 invalid_token for a caller with no accepted key — sign in, or
//     check the key you sent;
//   - 403 forbidden for a caller whose key was accepted and reaches less —
//     a member on an admin surface. Signing in again would change nothing.
//
// A reach that is not one of [Reaches] panics at mount, never at request:
// a route that cannot say who may reach it is a wiring mistake, and serving
// it to anybody — or to nobody — would hide that behind behaviour.
func (g *Guard) Require(need Reach, next http.Handler) http.Handler {
	if !need.Valid() {
		panic("auth: a route was mounted with reach " + string(need) + ", which is not one of auth.Reaches")
	}
	if need == ReachOpen {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := PrincipalFrom(r.Context())
		if p.Reach.Covers(need) {
			next.ServeHTTP(w, r)
			return
		}
		if !p.Authenticated() {
			reason := "missing_key"
			if rejected, _ := r.Context().Value(rejectedKey{}).(bool); rejected {
				reason = "rejected_key"
			}
			log.Warn("api_auth_failed",
				"route", r.URL.Path, "reason", reason, "needs", string(need),
				// The candidate value is NEVER logged: a rejected key is
				// still a credential, and a log is a place it would
				// outlive the request.
				"remote", remoteHost(r))
			httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
			return
		}
		log.Info("api_reach_refused",
			"route", r.URL.Path, "token_id", p.ID, "reach", string(p.Reach), "needs", string(need))
		httpjson.Fail(w, http.StatusForbidden, httpjson.CodeForbidden)
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
