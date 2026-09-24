package operator

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tracker"
)

// requestKey is the context key [WithRequestKey] stores under. An unexported
// type, so no other package can write or shadow it.
type requestKey struct{}

// WithRequestKey marks ctx as carrying the caller's own idempotency key for
// ONE request — the identity a retry of that request repeats.
//
// A TRANSPORT SETS IT, never a tool argument: the key decides which writes
// the operation ledger collapses into one, and a caller who could put it in
// the arguments of an ordinary call could collapse somebody else's write by
// guessing theirs. A transport that knows no request identity — MCP, whose
// calls name none — sets nothing, and every write it makes is fresh.
//
// An empty key is no key: stored as given and read back as "", which every
// consumer treats exactly as a request that named none.
func WithRequestKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, requestKey{}, key)
}

// requestKeyFrom reads the key [WithRequestKey] stored, or "".
func requestKeyFrom(ctx context.Context) string {
	key, _ := ctx.Value(requestKey{}).(string)
	return key
}

// WorkActor and PageActor read the operator off the request's context.
//
// # An unbound token identifies as itself
//
// A Tier A token has a NAME — the key in `api.auth.tokens` — and that name is
// what lands on the record: `founder`, `ci`, `ops-bot`. It is not a seat and
// must not look like one, so the actor KIND is [tracker.AuthorOperator] — the
// discriminator every renderer and every recipient rule already reads — and
// the credential is recorded again in `OperatorID` so an audit can ask what
// one token did without reasoning about kinds.
//
// The name goes in the author field rather than being left empty because the
// tracker requires one: a record carrying no author is a history row nobody
// can attribute, which is the single thing this surface exists to prevent.
//
// The alternative — asking the caller to name a seat to act as — was rejected:
// it lets anybody with the token write as anybody, and a tracker whose author
// field can be chosen by the writer is not an audit trail.
//
// # A BOUND token also says who it IS, and that is a different field
//
// `contact.crewlet_operator_id` binds a token to a human seat, and that
// binding is an ATTRIBUTION rather than an address — so it changes nothing
// above. What it answers is the question the rule above does not: whose inbox,
// whose pins, whose queue, whose day. Those are the PERSON's, and the person
// is the seat. So the seat travels in [builtin.Actor.Seat], beside an author
// that is still the token and a kind that is still `operator`.
//
// Left unresolved, the person tools wrote a second person record named after
// the credential and `get_my_work` answered for a party of one that no
// colleague had ever filed anything against.
//
// A CONSTRUCTOR because the resolution needs the chart, which is a Tier B
// value a config apply replaces — so it is read per call rather than captured.
// A nil chart, or a build with none loaded, resolves no seat, which is exactly
// an unbound token and an ordinary state.
func WorkActor(chart func() *org.Organization) func(
	context.Context, *turnctx.Turn) (builtin.Actor, error) {

	return func(ctx context.Context, _ *turnctx.Turn) (builtin.Actor, error) {
		id, ok := auth.OperatorFrom(ctx)
		if !ok || id == "" {
			return builtin.Actor{}, fmt.Errorf("operator: no operator on this request")
		}
		// THE OPERATOR'S OWN NAME IS THE HANDLE, and the kind says it
		// is not a seat. A tracker whose author field is chosen by the
		// writer is not an audit trail, so there is deliberately no way
		// for a caller to name a seat to act as.
		actor := builtin.Actor{
			Handle: id, Kind: tracker.AuthorOperator, OperatorID: id,
		}
		actor.Seat = seatFor(chart, id)
		// AND THE REQUEST'S OWN KEY, where the transport named one: it
		// is what makes a retried write the same operation. See
		// [WithRequestKey].
		actor.RequestKey = requestKeyFrom(ctx)
		return actor, nil
	}
}

// seatFor is the chart seat a token id is bound to, or "".
//
// NIL LOOKUP, so a `crewlet_operator_id: ${FOUNDER_ID}` resolves against this
// process's own environment — which is where every other consumer of `contact`
// resolves one, and what stops a company being bound for one direction and
// unbound for the other. The other direction is [builtin.Parties].
func seatFor(chart func() *org.Organization, id string) string {
	if chart == nil {
		return ""
	}
	o := chart()
	if o == nil {
		return ""
	}
	seat := o.SeatByOperatorID(id, nil)
	if seat == nil {
		return ""
	}
	return seat.Handle()
}

// PageActor is [WorkActor] for the knowledge base, and records the SAME
// operator under the SAME name.
//
// THE HANDLE IS THE TOKEN'S OWN NAME, exactly as above. It used to be left
// empty here, and `pages.Actor.Name` falls back to `"operator:" + OperatorID`
// for an actor with no handle — so one person writing through one surface was
// recorded as `founder` on a work commit and `operator:founder` on a page
// commit. The kind is already on the row, in its own column, so the prefix was
// a second encoding of a fact the row carries; what it bought was that the
// audit feed, which is the one screen that reads both histories, showed the
// same person as two people three rows apart, and that a reader filtering on
// a name matched half of what they did.
func PageActor(ctx context.Context, _ *turnctx.Turn) (pages.Actor, error) {
	id, ok := auth.OperatorFrom(ctx)
	if !ok || id == "" {
		return pages.Actor{}, fmt.Errorf("operator: no operator on this request")
	}
	return pages.Actor{Handle: id, Kind: pages.AuthorOperator, OperatorID: id}, nil
}
