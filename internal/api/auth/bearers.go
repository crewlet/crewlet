package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/credential"
)

// A PRESENTED BEARER IS AN ATTEMPT, AND IT MEETS A CURVE.
//
// Every guarded route compares the bearer it is handed against the Tier A
// entries and the identity directory's machine tokens, and whether the
// comparison matched is the whole of what the caller learns: 401, or the
// route's own answer. With nothing in front of that comparison it was a
// guessing oracle at line rate, on every route this engine serves — the sign-in
// surface's throttle covered the password routes and nothing else, so a spray
// of bearers at `POST /auth/token` or any read was answered as fast as it
// arrived. The values are long and random, which is the protection; this is
// the defence in depth the password routes already had.
//
// # The source, and nothing else
//
// A bearer names nobody until it is compared, so there is no subject to key a
// pair on: the curve is [credential.Throttle]'s SOURCE curve alone — ten
// refused bearers in the window free, then a wait that doubles from a second
// to thirty, a wait up to five seconds served inside the request and a longer
// one `429 throttled` carrying a `Retry-After`. Never a lockout, for the
// sign-in throttle's reason: a refusal after N is a lockout anybody can cause.
//
// ADMITTED BEFORE THE COMPARISON, because the comparison is what a guesser
// wants: a curve consulted afterwards would let a correct guess through
// whatever the curve said. So a valid bearer from a source that is spraying is
// made to wait with it — which is the price of the source being the only key
// there is, and why `api.trusted_proxies` matters here as much as on the
// sign-in routes.
//
// A SUCCESS CLEARS NOTHING. A bearer that matched proves the caller holds one
// credential, not that the other values from their address were theirs: a
// success that flushed the source would let anybody holding a token wipe the
// record of their guesses at another by presenting their own between them.
// It only resolves its own attempt.
//
// # LOCAL ONLY, on purpose
//
// This curve is the node's own and never touches the coordination store, which
// is the opposite of the sign-in pair's and deliberate: a Tier A token is the
// documented way back in when the identity provider or the coordination store
// is down, and a shared window a spray could fill would make that way back in
// something an attacker can close fleet-wide. What a load balancer spreading a
// spray across nodes costs is one curve per node, which still bounds it.
//
// # Only where the guard relies on the credential
//
// The curve applies on a GUARDED route and nowhere else — the same line the
// audit trail draws (see audit.go): an unguarded route's bearer is not this
// guard's to judge, and the Forge relay's own JWT on /webhooks/forge would
// otherwise put the relay on the curve with every delivery. That line holds
// only while no unguarded route ANSWERS DIFFERENTLY for a good bearer and a
// bad one, which is why the provider step-up start refuses every presented
// bearer alike ([PresentedBearer]).
//
// An attempt still being compared counts as a refusal until it resolves, so a
// burst of concurrent guesses is served along the curve rather than all at
// once. A comparison takes microseconds, so an honest client is slowed only if
// it has more than ten in flight at one instant from one address — and then by
// a second, never refused.

// bearerAttempt is one presented bearer the curve admitted, until its
// comparison decides it. The zero value is a request the curve does not judge,
// and its nil ticket does nothing.
type bearerAttempt struct {
	ticket *credential.Ticket
}

// admitBearer runs the bearer curve for a request, answering false once it has
// written the refusal.
func (g *Guard) admitBearer(w http.ResponseWriter, r *http.Request) (bearerAttempt, bool) {
	if g.bearers == nil || Unguarded(r.URL.Path) || g.Credential(r) == "" {
		return bearerAttempt{}, true
	}
	client := g.Client(r)
	ticket, err := g.bearers.Admit(r.Context(), credential.Attempt{Source: client})
	if err == nil {
		return bearerAttempt{ticket: ticket}, true
	}
	if errors.Is(err, credential.ErrThrottled) {
		// COUNTED, NEVER LOGGED: whoever is spraying authors the rate of
		// these, and the counter and the per-minute row are where a rate
		// an outsider controls is allowed to land.
		if g.audit != nil {
			g.audit.Failed(r.Context(), authevents.Failure{
				Client: client, Method: types.FailBearer, Throttled: true,
			})
		}
		httpjson.Throttled(w, credential.RetryAfter(err))
		return bearerAttempt{}, false
	}
	// THE WAIT ENDED WITH THE REQUEST — the caller went away, or this node
	// is shutting down — and nothing was compared.
	httpjson.Unavailable(w, httpjson.CodeUnavailable, RetryIdentitySeconds)
	return bearerAttempt{}, false
}

// settle resolves an admitted bearer once the comparison has spoken: a bearer
// that matched is a success, one the resolution refused is a failure, and one
// this node could not check ([iam.Unknown]) is neither — released by the
// caller's deferred [bearerAttempt.release], so an outage never puts a good
// credential's address on the curve.
func (a bearerAttempt) settle(ctx context.Context, r *http.Request, how iam.Resolution) {
	switch {
	case how == iam.Resolved:
		a.ticket.Succeed(ctx)
	case how == iam.Anonymous && refusedBearer(r):
		a.ticket.Fail(ctx)
	}
}

// release resolves an attempt nothing else did as nothing at all.
func (a bearerAttempt) release() { a.ticket.Release() }

// refusedBearer reports whether r's resolution refused the credential it
// presented.
func refusedBearer(r *http.Request) bool {
	_, marked := r.Context().Value(refusedKey{}).(string)
	return marked
}

// bearerKey marks a request that presented a bearer, whatever became of it.
type bearerKey struct{}

// withBearer marks ctx as carrying a presented bearer.
func withBearer(ctx context.Context) context.Context {
	return context.WithValue(ctx, bearerKey{}, true)
}

// PresentedBearer reports whether the request presented a bearer at all —
// matched, refused or uncheckable alike.
//
// FOR AN UNGUARDED ROUTE THAT READS THE RESOLUTION, which has to answer a good
// bearer and a bad one the same way: the bearer curve applies only where the
// guard relies on a credential, so an unguarded route whose answer said which
// bearers matched would be an oracle nothing throttles. The provider step-up
// start is that route, and it refuses every presented bearer alike.
func PresentedBearer(ctx context.Context) bool {
	presented, _ := ctx.Value(bearerKey{}).(bool)
	return presented
}
