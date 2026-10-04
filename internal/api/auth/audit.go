package auth

import (
	"bufio"
	"context"
	"net"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam/authevents"
)

// WHAT THE GUARD TELLS THE AUDIT TRAIL, and which of it is allowed to be a row.
//
// The guard sees every request, which makes it the frame that knows that a
// credential was presented and REFUSED. That is authored by whoever holds the
// wrong value, which is anybody who can reach the listener, so it is never an
// event: it is a failed attempt of method `bearer`, counted, and folded into
// what the engine's own loop publishes once each minute has closed
// (internal/iam/authevents).
//
// A TIER A TOKEN'S USE IS NOT A ROW. Every write a token makes already names
// it — the author and `operator_id` columns every record carries — so a row
// per first use in an hour said again, coarsely, what the records say exactly,
// and took a bounded once-per-window set of its own to say it. A route
// REFUSING a token — something holding it reaching past its grants — is a
// WARN log line on this node ([Guard.overreached]), which an operator's log
// alerting sees without a dedupe of its own.
//
// BOTH ARE A REQUEST'S, recorded by the middleware alone. An open socket
// decides the credential it was opened with again whenever the identity
// estate moves it ([Guard.ResolveOpen]), outside the middleware, and a
// re-decision is not somebody presenting a credential: counted, every tab a
// revocation closed would be a failed attempt on the per-minute row.
//
// # A refusal is an attempt only where the guard RELIED on the credential
//
// Resolve runs on every request, the [Unguarded] ones included, and those
// carry credentials that were never this guard's to check: the Atlassian Forge
// relay's own `Authorization: Bearer` JWT on /webhooks/forge, verified by that
// route against the relay's keys; a sign-in page loaded by a browser that
// still holds a cookie signed under a retired key. Counted at the resolution,
// every legitimate Jira and Confluence delivery was a failed bearer sign-in,
// and any alert on the counter fired on ordinary traffic. So Resolve only
// MARKS what it refused ([refusedCredential]) and the middleware counts the
// mark in the one arm where the refusal decided something: a guarded route
// answering 401. The credential exchange at `POST /auth/token` is such a
// route, and so is the socket's handshake.

// Audit is where the guard's authentication facts go.
//
// CONSUMER-DEFINED and one method wide, which is all of the trail the guard
// uses. internal/iam/authevents' Trail is what a running node hands in.
type Audit interface {
	Failed(ctx context.Context, f authevents.Failure)
}

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

// refusedKey marks a request whose presented credential [Guard.Resolve]
// checked and turned away, from the resolution to the one arm of the
// middleware that counts it.
type refusedKey struct{}

// refusedCredential marks a request whose presented credential this node
// checked and refused — a bearer no entry, token or row accepts, or a cookie
// that is not a bearer of this format at all.
//
// A MARK AND NOT A COUNT: whether the refusal is somebody's failed attempt
// depends on whether the route needed the credential, which the resolution
// does not know and the middleware does. See the file doc. And ONLY a mark:
// what was presented is not carried, because nothing the trail keeps is
// about it — a failure is counted per source.
func refusedCredential(ctx context.Context) context.Context {
	return context.WithValue(ctx, refusedKey{}, true)
}

// refused counts the credential a request's resolution refused, if it
// refused one — called by the middleware on a guarded route's 401 and
// nowhere else.
func (g *Guard) refused(r *http.Request) {
	marked, _ := r.Context().Value(refusedKey{}).(bool)
	if g.audit == nil || !marked {
		return
	}
	g.audit.Failed(r.Context(), authevents.Failure{
		Source: g.Client(r), Method: types.FailBearer,
	})
}

// overreached logs a Tier A token a route refused with 403: the token matched
// and was not enough, so something holding it reached past what it was pinned
// for — the question a break-glass credential's owner most wants answered.
//
// A LOG LINE AND NOT AN EVENT: an event per refusal is a rate the token's
// holder chooses, and one coalesced per token per hour took a bounded dedupe
// set to keep — while a WARN line is what an operator's log alerting already
// reads, at whatever rate it arrives.
func (g *Guard) overreached(r *http.Request, entry config.APIToken, status int) {
	log.WarnContext(r.Context(), "api_auth_token_overreach",
		"token", entry.ID, "route", r.URL.Path, "status", status,
		"remote", g.Client(r),
		"detail", "a Tier A token reached for a route its grants do not cover")
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
// tell a Tier A token's overreach after the route has decided.
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
// [http.Hijacker], which is how the dashboard's socket upgrades behind this
// wrapper.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return http.NewResponseController(w.ResponseWriter).Hijack()
}
