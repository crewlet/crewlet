package auth

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam/authevents"
)

// WHAT THE GUARD TELLS THE AUDIT TRAIL, and which of it is allowed to be a row.
//
// The guard sees every request, which makes it the frame that knows three
// things nothing else does: that a credential was presented and REFUSED, that a
// Tier A token was USED, and that a Tier A token reached for something its
// grants did not cover. They reach the trail by the two doors
// internal/iam/authevents keeps apart:
//
//   - A REFUSED CREDENTIAL is authored by whoever holds the wrong value, which
//     is anybody who can reach the listener. It is never an event: it is a
//     failed attempt of method `bearer`, counted, and folded into the one row
//     per client per minute the engine's own loop publishes.
//   - A USE and an OVERREACH are authored by the token's holder — the token
//     matched, so there is a credential to revoke and a name on the row — and
//     they are events, COALESCED to one per token per [TokenUseWindow] unless
//     the token's own entry sets `audit_every_use`.
//
// USE AND OVERREACH ARE A REQUEST'S, and are recorded by the middleware alone.
// [Guard.Resolve] also runs when an open socket re-checks the credential it was
// opened with, once a minute, and a re-check is not somebody using a token: a
// per-request audit that counted it would write a row a minute for every tab
// left open.

// Audit is where the guard's authentication facts go.
//
// CONSUMER-DEFINED and three methods wide, which is all of the trail the guard
// uses. internal/iam/authevents' Trail is what a running node hands in.
type Audit interface {
	Emit(ctx context.Context, payload events.Payload)
	EmitOnce(ctx context.Context, key string, window time.Duration,
		payload events.Payload) bool
	Failed(ctx context.Context, f authevents.Failure)
}

// TokenUseWindow is how often a Tier A token's use, or its overreach, is a row.
//
// AN HOUR, the design's own figure and the notification digest's idiom: one
// row stands for the window, so "was the break-glass token used today, and
// from where" has an answer while an assistant driving the operator surface —
// a request per tool call — adds a row an hour rather than thousands. A token
// that needs every request recorded says so at its own entry with
// `audit_every_use`, which is where a credential's blast radius is stated.
const TokenUseWindow = time.Hour

// WithAudit installs the trail the guard reports to, and returns the guard for
// chaining.
//
// CALLED ONCE, AT WIRING TIME, for [Guard.BindSeats]' reason: it is read on
// every request. Nil is what a guard built for a suite about something else
// has, and it records nothing — internal/api refuses to build an App without
// one, which is where a real node's guard comes from.
func (g *Guard) WithAudit(a Audit) *Guard {
	g.audit = a
	return g
}

// tierAKey carries the Tier A entry a request's bearer matched, from the
// resolution to the middleware that records its use.
type tierAKey struct{}

// withTierA marks a request as authenticated by a Tier A token.
func withTierA(ctx context.Context, entry config.APIToken) context.Context {
	return context.WithValue(ctx, tierAKey{}, entry)
}

// tierAOf is the Tier A entry a request authenticated with, if it did.
func tierAOf(ctx context.Context) (config.APIToken, bool) {
	entry, ok := ctx.Value(tierAKey{}).(config.APIToken)
	return entry, ok && entry.ID != ""
}

// refused records a presented credential this node checked and turned away.
//
// THE VALUE GOES IN AS THE SUBJECT and no further: the trail keys it under a
// secret of its own the moment it arrives, so what leaves the process is how
// many DIFFERENT values one client sprayed in a minute, never any of them.
func (g *Guard) refused(r *http.Request, presented string) {
	if g.audit == nil {
		return
	}
	g.audit.Failed(r.Context(), authevents.Failure{
		Client: g.Client(r), Method: types.FailBearer, Subject: presented,
	})
}

// used records a Tier A token's use: the first in its window, or every one
// where its entry asks for that.
func (g *Guard) used(r *http.Request, entry config.APIToken) {
	if g.audit == nil {
		return
	}
	row := types.IAMTokenFirstUse{
		Token: entry.ID, Route: RouteClass(r.URL.Path), Remote: g.Client(r),
		EveryUse: entry.AuditEveryUse,
	}
	if entry.AuditEveryUse {
		g.audit.Emit(r.Context(), row)
		return
	}
	g.audit.EmitOnce(r.Context(), "token_first_use:"+entry.ID, TokenUseWindow, row)
}

// overreached records a Tier A token a route refused.
func (g *Guard) overreached(r *http.Request, entry config.APIToken, status int) {
	if g.audit == nil {
		return
	}
	row := types.IAMTokenOverreach{
		Token: entry.ID, Route: RouteClass(r.URL.Path), Remote: g.Client(r),
		Status: status, EveryUse: entry.AuditEveryUse,
	}
	if entry.AuditEveryUse {
		g.audit.Emit(r.Context(), row)
		return
	}
	g.audit.EmitOnce(r.Context(), "token_overreach:"+entry.ID, TokenUseWindow, row)
}

// RouteClass is the part of a path an audit row names: its first segment.
//
// NOT THE PATH, which carries ids — a person, a task, a secret's name — that a
// feed of token uses has no business collecting, and which would make every
// row about the same surface unique. The first segment is the SURFACE
// (`/secrets`, `/iam`, `/config`), which is the question "what was this token
// used for" actually asks.
func RouteClass(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		trimmed = trimmed[:i]
	}
	return "/" + trimmed
}

// refusalStatus reports whether a status is a refusal of AUTHORITY, which is
// what an overreach is: the credential matched and was not enough.
//
// 403 ALONE. A 401 is a credential that did not match, which is not this
// token; a 404 is a route that does not exist, which no grant would have
// opened; and a 503 is a node that could not decide, which is nobody's
// overreach.
func refusalStatus(status int) bool { return status == http.StatusForbidden }

// statusWriter remembers the status a handler answered with, so the guard can
// tell a Tier A token's overreach from its use after the route has decided.
//
// IT WRAPS ONLY A TIER A REQUEST. Every other request passes through the
// writer it arrived with, so a WebSocket upgrade or a streaming response on a
// session is not asked to survive an extra layer it has no use for; a Tier A
// socket keeps the original writer's Hijacker and Flusher through the two
// methods below.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the writer underneath — its
// Flush, its Hijack for a socket upgrade, its deadlines — which a wrapper
// that hid them would silently take away from every route a Tier A token
// calls.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush passes a flush through for a handler that type-asserts rather than
// using a ResponseController.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack passes a socket upgrade through for a library that type-asserts
// [http.Hijacker], which is how the dashboard's socket reaches a script that
// presents a Tier A token on its query string.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return http.NewResponseController(w.ResponseWriter).Hijack()
}
