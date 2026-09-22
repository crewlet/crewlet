package auth

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
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

// TokenLoginPrefix is the class segment a Tier A token's login carries.
//
// internal/iam's machine grammar joins segments with a COLON, which is the one
// separator a seat handle can never contain and a person's dotted login never
// uses — so `token:ops` cannot collide with either namespace by construction.
// It is what [iam.ActorFor] writes into an audit row, and it is deliberately
// the WHOLE name rather than a prefix the store strips: the author kind is
// already a column, and a name half the rows carry a prefix on is a name a
// reader filtering on it matches half of.
const TokenLoginPrefix = "token:"

// TokenLogin is the login a Tier A token acts under.
func TokenLogin(id string) string { return TokenLoginPrefix + id }

// principalFor composes the principal a Tier A token acts as.
//
// THE CEILING IS APPLIED HERE, at the moment the request is resolved, which is
// what `api.auth.max_grants` being a decision-time bound means: nothing was
// written when it changed, so lowering it takes effect on the next request
// this node serves. A token declaring a grant the ceiling withholds is refused
// by `crewlet validate` — this is the second half, for the node whose ceiling
// is lower than the one the document was validated against, which is every
// node mid-rollout.
func (g *Guard) principalFor(entry config.APIToken, now time.Time) iam.Principal {
	p := iam.Principal{
		ID:        uuid.NewSHA1(TokenNamespace, []byte(entry.ID)),
		Login:     TokenLogin(entry.ID),
		Kind:      iam.KindMachine,
		Grants:    intersect(entry.Grants, g.ceiling),
		Colleague: entry.Level(),
		// PRESENTING THE TOKEN IS THE PROOF, so it is fresh for the
		// ordinary step-up window.
		//
		// There is nothing else it could present: a Tier A token has no
		// second factor, no session and no person behind it. A
		// credential that could never be fresh would be one that could
		// never reach a sensitive gesture — which is precisely the job
		// break-glass exists for, on the day the identity provider is
		// down and an administrator is locked out.
		ReauthAt: now.Add(g.stepUp),
		Stage:    iam.StageActive,
	}
	// A BOUND CREDENTIAL ACTS AS ITS SEAT, which is the one thing this
	// translation is not blunt about. The binding is the company's
	// (`contact.crewlet_operator_id` on a human seat) and it already
	// means exactly this: somebody at the dashboard acts as themselves
	// rather than as the credential they hold. It changes the KIND as
	// well as the name, because [iam.ActorFor] splits a person on their
	// seat and a machine has no such split — a write from a bound
	// credential lands under a person's own handle.
	if g.boundSeat != nil {
		if handle := g.boundSeat(entry.ID); handle != "" {
			p.Kind, p.Seat = iam.KindPerson, handle
		}
	}
	return p
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

// Resolve answers who a request is, three-valued.
//
// THE THREE ARMS ARE THE POINT, and they are not the same as the two
// [Guard.Presented] answers:
//
//   - a credential that names somebody is [iam.Resolved];
//   - no credential at all, or one this node refuses, is [iam.Anonymous] — a
//     finding, and the arm a guarded route answers 401 to;
//   - a credential this node could not CHECK is [iam.Unknown], which no
//     path reaches yet because a Tier A token is a map in memory. It exists
//     because the session lookup that arrives next can fail, and a caller
//     written against two arms would then read "the store is down" as "you
//     are nobody" — and go on reading it that way for as long as the outage
//     lasted, teaching everybody to go and check their password.
//
// It attaches the answer to the context rather than returning it, because
// [iam.From] is what every surface downstream reads and a second channel would
// be a second answer.
func (g *Guard) Resolve(r *http.Request) *http.Request {
	candidate := g.Credential(r)
	if candidate == "" {
		return r.WithContext(iam.WithAnonymous(r.Context()))
	}
	entry, ok := g.entry(candidate)
	if !ok {
		// PRESENT AND WRONG IS STILL ANONYMOUS, not unknown: this node
		// checked and the answer was no. Unknown is reserved for the
		// question it could not ask.
		return r.WithContext(iam.WithAnonymous(r.Context()))
	}
	return r.WithContext(iam.WithPrincipal(r.Context(), g.principalFor(entry, g.now())))
}
