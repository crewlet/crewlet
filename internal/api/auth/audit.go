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
// ALL THREE ARE A REQUEST'S, and are recorded by the middleware alone.
// [Guard.Resolve] also runs when an open socket re-checks the credential it was
// opened with, once a minute, and a re-check is not somebody using a token: a
// per-request audit that counted it would write a row a minute for every tab
// left open.
//
// # A refusal is an attempt only where the guard RELIED on the credential
//
// Resolve runs on every request, the [Unguarded] ones included, and those
// carry credentials that were never this guard's to check: the Atlassian Forge
// relay's own `Authorization: Bearer` JWT on /webhooks/forge, verified by that
// route against the relay's keys; a sign-in page loaded by a browser that
// still holds a cookie signed under a retired key. Counted at the resolution,
// every legitimate Jira and Confluence delivery was a failed bearer sign-in —
// a distinct value each time, so the relay's address climbed toward a spray's
// distinct-name count every minute, and any alert on the counter fired on
// ordinary traffic. So Resolve only MARKS what it refused ([refusedCredential])
// and the middleware counts the mark in the one arm where the refusal decided
// something: a guarded route answering 401. The credential exchange at
// `POST /auth/token` is such a route, and so is the socket's handshake.

// Audit is where the guard's authentication facts go.
//
// CONSUMER-DEFINED and four methods wide, which is all of the trail the guard
// uses. internal/iam/authevents' Trail is what a running node hands in, and
// the once-per-window classes are its own: each keeps a bounded set of its
// own, so no class can evict another's keys. Claim is EmitOnce's decision
// without the row, for the deadline arm, which has to read the estate before
// it knows whether there is anything to say.
type Audit interface {
	Emit(ctx context.Context, payload events.Payload)
	EmitOnce(ctx context.Context, class authevents.OnceClass, key string,
		window time.Duration, payload events.Payload) bool
	Claim(ctx context.Context, class authevents.OnceClass, key string,
		window time.Duration) (release func(), claimed bool)
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

// tierAKey carries the Tier A entry a request's authority rests on, from the
// resolution to the middleware that records its use and to the one route that
// has to know which credential SHAPE arrived.
type tierAKey struct{}

// tierAUse is what a request's Tier A authority is, and how it arrived.
type tierAUse struct {
	entry config.APIToken

	// presented is true when the request carried the token's own VALUE as
	// its bearer, and false when it carried a session exchanged from it.
	presented bool
}

// withTierA marks a request as acting on a Tier A token's authority.
func withTierA(ctx context.Context, entry config.APIToken, presented bool) context.Context {
	return context.WithValue(ctx, tierAKey{}, tierAUse{entry: entry, presented: presented})
}

// TierA is the Tier A entry a request's authority rests on, if it rests on
// one — whether the request presented the token itself or a session the token
// was exchanged for.
//
// ONE ANSWER FOR BOTH SHAPES, because the entry is the authority either way:
// the session carries nothing of its own and is re-composed from the entry on
// every request, so a use of it is a use of the token and an overreach through
// it is the token's overreach.
func TierA(ctx context.Context) (config.APIToken, bool) {
	use, ok := ctx.Value(tierAKey{}).(tierAUse)
	return use.entry, ok && use.entry.ID != ""
}

// PresentedTierA is the Tier A entry whose VALUE this request carried as its
// bearer, if it carried one — and never the entry behind an exchanged
// session.
//
// THE ONE ROUTE THAT ASKS is the exchange itself: it turns a token's value into
// a session, and a session presented to it has no value to exchange.
func PresentedTierA(ctx context.Context) (config.APIToken, bool) {
	use, ok := ctx.Value(tierAKey{}).(tierAUse)
	return use.entry, ok && use.presented && use.entry.ID != ""
}

// refusedKey carries the credential [Guard.Resolve] checked and turned away,
// from the resolution to the one arm of the middleware that counts it.
type refusedKey struct{}

// refusedCredential marks a request whose presented credential this node
// checked and refused — a bearer no entry, token or row accepts, or a cookie
// that is not a bearer of this format at all.
//
// A MARK AND NOT A COUNT: whether the refusal is somebody's failed attempt
// depends on whether the route needed the credential, which the resolution
// does not know and the middleware does. See the file doc.
//
// THE VALUE RIDES ON THE REQUEST'S OWN CONTEXT under an unexported key, and
// goes no further than the trail, which keys it under a secret of its own
// the moment it arrives — so what leaves the process is how many DIFFERENT
// values one client sprayed in a minute, never any of them.
func refusedCredential(ctx context.Context, presented string) context.Context {
	return context.WithValue(ctx, refusedKey{}, presented)
}

// refused counts the credential a request's resolution refused, if it
// refused one — called by the middleware on a guarded route's 401 and
// nowhere else.
func (g *Guard) refused(r *http.Request) {
	presented, marked := r.Context().Value(refusedKey{}).(string)
	if g.audit == nil || !marked {
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
	g.audit.EmitOnce(r.Context(), authevents.OnceTokenUse, entry.ID,
		TokenUseWindow, row)
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
	g.audit.EmitOnce(r.Context(), authevents.OnceTokenOverreach, entry.ID,
		TokenUseWindow, row)
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
