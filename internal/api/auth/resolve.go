package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// HOW A TIER A TOKEN BECOMES A PRINCIPAL, and why the conversion lives here
// rather than at each surface that needs one.
//
// A Tier A token is the DEPLOYMENT's own machine credential: break-glass, the
// operator CLI, a pipeline. It is not a person and is never a stand-in for
// one. What every surface downstream needs is the same thing a session will
// hand it once one exists — an [iam.Principal] — so the two credential shapes
// meet HERE and nowhere else, and no route has to know which one arrived.
//
// Every field below is derived from something the configuration already says.
// Nothing is read from a store, which is what lets this run before any node
// has applied anything.

// TokenNamespace is the UUIDv5 namespace a Tier A token's principal id is
// derived under.
//
// DERIVED RATHER THAN MINTED, for the reason internal/org derives a seat's
// id: every node in the fleet computes the same id for the same token with no
// database and no coordination, so an audit row written on one node resolves
// to the same principal on every other. A minted id would differ per process
// and make one credential look like as many principals as there are nodes.
//
// A FIXED NAMESPACE rather than a per-deployment one, because the id has to be
// stable across a restart and a node rebuild, and the only inputs that are
// stable across both are this constant and the token's own id.
var TokenNamespace = uuid.MustParse("6f1c2d5e-9a34-5b7c-8e10-4d2f6a8b3c91")

// SeatBindings is what a Tier A token's seat binding is resolved through.
//
// # Two halves, and the second is the SESSION's
//
// Directory answers what the identity directory says about the token's login:
// the seat its row is bound to and the chart position that binding was decided
// at. Chart answers whether that seat is one the credential may act as NOW —
// and it is the same [session.Chart] a signed-in person's binding is resolved
// through, by the same [session.ResolveSeat], so a token and a cookie bound to
// one seat are told the same thing about it: served under the handle the chart
// answers to after a rename, refused 403 `seat_unavailable` NAMING the seat
// once it is removed, tombstoned or not a human seat, and 503 while this node
// has not applied the chart as far as the binding or has stalled past the
// grace.
//
// That is the whole reason this is not a lookup returning a handle. The one it
// replaced was, and a token then went on acting as whatever handle a row
// held: a seat the chart had removed, a seat renamed out from under it, an
// agent's seat, and — since a key is not an identity — a handle the chart had
// since given to somebody else, whose lead relations the token then carried.
//
// # Zero means no binding source
//
// The zero value binds nothing, and every token acts as the bare credential,
// which is what an API built without an identity directory — a suite standing
// a handler up — honestly has. A Directory with no Chart is not that: it is a
// binding that cannot be resolved, and every bound token answers 503, which is
// [session.ResolveSeat]'s own reading of a missing chart.
type SeatBindings struct {
	// Directory reads the identity directory's row for a login. Nil binds
	// nothing.
	Directory BindingDirectory

	// Chart is the org chart view a binding is resolved in.
	Chart session.Chart
}

// BindingDirectory is the one read a Tier A token's seat binding needs.
//
// DEFINED HERE, by the caller, and one method wide.
type BindingDirectory interface {
	// BoundSeat answers the row a machine credential's login binds through,
	// as the seat table reads a holder: `Found` with a seat and the chart
	// position it was decided at, or the zero row for a credential nobody
	// binds. AN ERROR IS THE UNKNOWN ARM — this node could not say — and
	// never "not bound", which would have a bound credential act as itself
	// here and as its seat on the next node: one actor under two names in
	// one audit trail.
	BoundSeat(ctx context.Context, login string) (session.PersonRow, error)
}

// errBindingUnavailable is the reason attached to a request whose Tier A
// token's seat binding this node could not resolve.
//
// A SENTENCE AND NOT A BARE SENTINEL, for [errIdentityUnavailable]'s reason:
// the gate downstream chooses a status from it, and it must read as an estate
// that could not be read rather than as a route nobody wired.
var errBindingUnavailable = errors.New("auth: this node could not resolve " +
	"which seat this credential is bound to")

// principalFor composes the principal a Tier A token acts as, three-valued
// exactly as a session is: resolved, resolved and refused its seat, or unknown.
//
// THE CEILING IS APPLIED HERE, at the moment the request is resolved, which is
// what `api.auth.max_grants` being a decision-time bound means: nothing was
// written when it changed, so lowering it takes effect on the next request
// this node serves. A token declaring a grant the ceiling withholds is refused
// by `crewlet validate` — this is the second half, for the node whose ceiling
// is lower than the one the document was validated against, which is every
// node mid-rollout.
func (g *Guard) principalFor(ctx context.Context, entry config.APIToken,
	now time.Time) (iam.Principal, iam.Resolution, *Refusal) {

	p := iam.Principal{
		ID:        uuid.NewSHA1(TokenNamespace, []byte(entry.ID)),
		Login:     iam.TokenLogin(entry.ID),
		Kind:      iam.KindMachine,
		Grants:    intersect(entry.Grants, g.ceiling),
		Colleague: entry.Level(),
		Stage:     iam.StageActive,
	}
	// PRESENTING THE TOKEN IS THE PROOF, so it is fresh for BOTH windows:
	// the sensitive one included.
	//
	// There is nothing else it could present: a Tier A token has no second
	// factor, no session and no person behind it. A credential that could
	// never be fresh would be one that could never reach a sensitive gesture
	// — which is precisely the job break-glass exists for, on the day the
	// identity provider is down and an administrator is locked out.
	g.proof.stamp(&p, now)
	// A BOUND CREDENTIAL ACTS AS ITS SEAT, which is the one thing this
	// translation is not blunt about. The binding is the IDENTITY
	// DIRECTORY's: a token enrolled there as a machine under its own
	// login and bound to a seat acts as that seat's holder rather than as
	// a bare credential. It changes the KIND as well as the name, because
	// [iam.ActorFor] splits a person on their seat and a machine has no
	// such split — a write from a bound credential lands under the seat.
	//
	// KEYED ON THE LOGIN, never the bare token id: the login is what the
	// directory claims arbitrate on (`iam.login.<login>`), and only a
	// MACHINE may hold `token:<id>`, which internal/iam's per-kind grammar
	// holds — so the row a token binds through is never one a person
	// chose.
	if g.bindings.Directory == nil {
		return p, iam.Resolved, nil
	}
	row, err := g.bindings.Directory.BoundSeat(ctx, p.Login)
	if err != nil {
		log.WarnContext(ctx, "api_token_binding_unavailable",
			"login", p.Login, "error", err)
		return iam.Principal{}, iam.Unknown, nil
	}
	if !row.Found || row.Seat == "" {
		return p, iam.Resolved, nil
	}
	// THE SESSION'S OWN TABLE, through the session's own chart seam — see
	// [SeatBindings] for what a second resolution cost.
	binding := session.ResolveSeat(ctx, g.bindings.Chart, row)
	switch binding.Answer() {
	case session.AnswerUnavailable:
		log.WarnContext(ctx, "api_token_seat_unavailable",
			"login", p.Login, "seat", row.Seat,
			"detail", binding.Detail, "error", errText(binding.Err))
		return iam.Principal{}, iam.Unknown, nil
	case session.AnswerServe:
		p.Kind, p.Seat = iam.KindPerson, binding.Handle()
		p.SeatAt, p.Position = row.SeatAt, binding.Seat.Unit
		return p, iam.Resolved, nil
	}
	// RESOLVED AND REFUSED, as a signed-in person bound to a gone seat is:
	// this node knows exactly which credential this is and will not say
	// what it acts as. It stays the MACHINE it is, with SeatAt beside an
	// empty seat — the pair that says "a binding was decided and this node
	// will not honour it" — and the guard writes the 403 everywhere but
	// the one surface that never reads a seat.
	p.SeatAt = row.SeatAt
	return p, iam.Resolved, seatRefusal(binding)
}

// intersect is the ceiling applied to a declared grant set.
//
// AN EMPTY CEILING GRANTS NOTHING, which is the opposite of how an empty list
// usually reads and is the only safe direction: a node whose configuration
// names no ceiling has not been told what the deployment allows, and admitting
// everything on that basis is how a rolling restart hands one node's callers
// authority the operator withdrew. Config refuses an absent ceiling on a node
// that serves, so reaching this with one is already a wiring mistake — and it
// fails closed rather than silently.
func intersect(declared, ceiling []iam.Grant) []iam.Grant {
	out := make([]iam.Grant, 0, len(declared))
	for _, g := range declared {
		for _, allowed := range ceiling {
			if g == allowed {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

// Resolve answers who a request is, three-valued, over the THREE credential
// shapes: a Tier A token (resolve.go), a machine token (tokens.go) and a
// session cookie (sessions.go).
//
// THE THREE ARMS ARE THE POINT, and they are not the same as the two
// [Guard.Presented] answers:
//
//   - a credential that names somebody is [iam.Resolved];
//   - no credential at all, or one this node refuses, is [iam.Anonymous] — a
//     finding, and the arm a guarded route answers 401 to;
//   - a credential this node could not CHECK is [iam.Unknown]: a session
//     whose rows it cannot read, or a Tier A token — itself a map in memory
//     — whose seat binding it cannot resolve. A caller written against two
//     arms would read "the store is down" as "you are nobody", and go on
//     reading it that way for as long as the outage lasted, teaching
//     everybody to go and check their password.
//
// It attaches the answer to the context rather than returning it, because
// [iam.From] is what every surface downstream reads and a second channel would
// be a second answer.
//
// THE RESPONSE WRITER IS TAKEN rather than a value returned, for one reason:
// resolving a SESSION can re-issue its cookie, and a re-issue that a caller
// had to remember to write is one a caller eventually forgets to — leaving a
// browser on a bearer whose idle deadline stops moving, signed out mid-work.
// Nothing else is written here; the refusal is returned.
func (g *Guard) Resolve(w http.ResponseWriter, r *http.Request) (
	*http.Request, *Refusal) {

	// THE DEVELOPMENT PRINCIPAL FIRST, and only for a request that
	// presented nothing. It is nil on every build that did not ask for it
	// and on every posture that refused it at construction, so this is a
	// nil check on an ordinary run. See devprincipal.go.
	if dev := g.devResolution(r); dev != nil {
		return r.WithContext(iam.WithPrincipal(r.Context(), *dev)), nil
	}
	// THE EXPLICIT CREDENTIAL BEFORE THE AMBIENT ONE. sessions.go argues
	// the order; what it buys is that a request presenting a WRONG bearer
	// stays anonymous rather than being quietly upgraded by whatever
	// cookie happened to be in the jar.
	if candidate := g.Credential(r); candidate != "" {
		entry, ok := g.entry(candidate)
		if !ok && g.machine != nil {
			// THE THIRD ARM, chosen by the value's SHAPE and never
			// gated on it: a value that parses is still verified, and
			// one that does not is refused below like any other. See
			// tokens.go.
			if presented, isToken := credential.ParseToken(candidate); isToken {
				return g.token(r, presented, candidate)
			}
		}
		if !ok {
			// PRESENT AND WRONG IS STILL ANONYMOUS, not unknown:
			// this node checked and the answer was no. Unknown is
			// reserved for the question it could not ask.
			//
			// AND IT IS MARKED, so a guarded route's refusal can count
			// it as a failed attempt — never a row of its own, because
			// whoever holds the wrong value decides how many of these
			// there are, and never counted HERE, because an unguarded
			// route's bearer was not this guard's to check. See
			// audit.go.
			return r.WithContext(refusedCredential(
				iam.WithAnonymous(r.Context()), candidate)), nil
		}
		principal, how, refusal := g.principalFor(r.Context(), entry, g.now())
		if how == iam.Unknown {
			// THE CREDENTIAL IS GOOD AND ITS SEAT CANNOT BE SAID, which
			// is 503 and never the bare credential: served as itself
			// here and as its seat on the next node, one actor would
			// author under two names in one audit trail.
			return r.WithContext(
				iam.WithUnresolved(r.Context(), errBindingUnavailable)), nil
		}
		ctx := iam.WithPrincipal(r.Context(), principal)
		return r.WithContext(withTierA(ctx, entry, true)), refusal
	}
	if g.sessions != nil {
		answer := g.sessions.resolve(w, r, g.ceiling, g.proof, g.Client,
			g.tokenByLogin)
		if answer.presented {
			if answer.tierA != nil && answer.how == iam.Resolved {
				return g.exchanged(r, *answer.tierA, answer.via)
			}
			if answer.how == iam.Resolved {
				return r.WithContext(
					iam.WithPrincipal(r.Context(), answer.principal)), answer.refusal
			}
			if answer.how == iam.Unknown {
				return r.WithContext(iam.WithUnresolved(r.Context(), errIdentityUnavailable)), answer.refusal
			}
			ctx := iam.WithAnonymous(r.Context())
			if answer.malformed != "" {
				ctx = refusedCredential(ctx, answer.malformed)
			}
			return r.WithContext(ctx), answer.refusal
		}
	}
	return r.WithContext(iam.WithAnonymous(r.Context())), nil
}

// exchanged is who a session exchanged from a Tier A token is: THE TOKEN, as
// the entry this node holds for it says now.
//
// # Composed from the configuration, never from a directory row
//
// The token has no row — it is a line in a config file — so the session it
// was exchanged for is validated against the ENTRY: [Sessions] has already
// refused one whose entry this node no longer holds, and this composes the
// principal through [Guard.principalFor], the one function the bearer itself
// is composed through. So the grants are the entry's cut to this node's
// ceiling on THIS request, a bound token acts as its seat exactly as the bearer
// would (and is refused it exactly as the bearer would), and the session is
// stepped up by construction for the reason the bearer is: presenting the
// token was the proof, and a break-glass session that could reach no
// sensitive surface would be no use on the day it exists for.
//
// The session was once composed from the directory's row for the token's
// DERIVED id, which no directory holds: once applied every request answered
// 401 and cleared the cookie, and before that it served a grantless nobody.
//
// WHAT IT DIFFERS FROM THE BEARER IN is the one thing that is not the token's:
// the credential the request came THROUGH, which is the session (via), and
// which the operator column records beside the token as the author — so a
// break-glass write from a browser names the sign-in that made it, as a
// person's does.
func (g *Guard) exchanged(r *http.Request, entry config.APIToken, via string) (
	*http.Request, *Refusal) {

	principal, how, refusal := g.principalFor(r.Context(), entry, g.now())
	if how == iam.Unknown {
		return r.WithContext(
			iam.WithUnresolved(r.Context(), errBindingUnavailable)), nil
	}
	principal.Via = via
	ctx := iam.WithPrincipal(r.Context(), principal)
	return r.WithContext(withTierA(ctx, entry, false)), refusal
}

// tokenByLogin is the Tier A entry a token's login names, or false.
//
// BY THE ID IN THE LOGIN and never by value: what an exchanged session carries
// is the token's NAME, and the entry this node holds under that name now is
// what it answers to — so removing the entry, or renaming it, ends every
// session exchanged from it on this node's next request.
func (g *Guard) tokenByLogin(login string) (config.APIToken, bool) {
	id, isToken := strings.CutPrefix(login, iam.TokenLoginPrefix)
	if !isToken || id == "" {
		return config.APIToken{}, false
	}
	entry, held := g.tokens[id]
	return entry, held
}
