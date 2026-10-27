package turnctx

import (
	"context"
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// WHAT A SEAT IS, TO THE AUTHORITY TABLE.
//
// A turn already knows which seat is acting; what internal/authz needs is an
// [iam.Principal], and until this existed nothing produced one for a seat at
// all — so every tool a seat called reached the decision with a context no
// resolver had answered for, which the three-valued read reports as UNKNOWN
// and a gate answers 503 to. An agent that could not read its own board is
// not a degraded company, it is a stopped one.

// Principal is who a turn acts as.
//
// THE SEAT'S HANDLE IS THE IDENTITY, and the kind is [iam.KindSeat] — which
// is what the authority table's own-record and colleague classes compare
// against, and what [iam.ActorFor] turns into an `agent` author. A turn with
// no seat answers the zero principal, which holds nothing and is refused by
// every class: a tool surface built outside a turn legitimately has no seat,
// and admitting it would make "no acting seat" the most powerful caller in
// the engine.
//
// # A seat holds every app and never admin
//
// EVERY APP, and not a per-seat choice, because what bounds a seat is its
// ROLE: a seat with no knowledge tools registered cannot write a page
// whatever it holds, so a permission that had to agree with the registry
// would be a second answer to one question. A company that wanted a seat to
// see nothing would not give it a seat.
//
// NEVER ADMIN. An agent that could rewrite the company it runs inside, read
// its credentials or every prompt every other agent was given, is the blast
// radius the vocabulary exists to bound — and the one permission a company
// might reasonably want to give a seat is precisely the one it must not be
// able to take. The two verbs only a seat makes — asking a colleague and a
// detached coding run — are the table's `seatOnly` rows rather than a
// permission a seat carries, so nothing beside the apps is needed here.
//
// A CLONE, so a decision that appends to the principal's grants can never
// reach the list of apps every other turn reads.
func Principal(t *Turn) iam.Principal {
	seat, err := t.RequireSeat()
	if err != nil {
		return iam.Principal{}
	}
	var o *org.Organization
	if t != nil {
		o = t.Org
	}
	return SeatPrincipal(o, seat)
}

// SeatPrincipal is who seat acts as in a company whose chart is o — the one
// construction of a seat's principal, which [Principal] makes for a turn's
// calls and the engine makes for what a turn is offered before any call, so
// the principal a surface is decided for and the one each call is decided as
// cannot differ. A nil seat is the zero principal, which holds nothing.
func SeatPrincipal(o *org.Organization, seat *org.Role) iam.Principal {
	if seat == nil {
		return iam.Principal{}
	}
	return iam.Principal{
		Kind:     iam.KindSeat,
		Seat:     seat.Handle(),
		Stage:    iam.StageActive,
		Grants:   slices.Clone(iam.Apps),
		Position: unitOf(o, seat),
	}
}

// unitOf is the unit a seat sits in, or empty.
//
// CARRIED ON THE PRINCIPAL rather than looked up per decision, for
// [iam.Principal.Position]'s own reason: a gate that re-walked the org chart
// per request would read a tree that has moved since the turn began.
func unitOf(o *org.Organization, seat *org.Role) string {
	if o == nil {
		return ""
	}
	unit := o.UnitFor(seat)
	if unit == nil {
		return ""
	}
	return unit.Key()
}

// WithPrincipal attaches the turn's seat as the acting party, unless
// something upstream has already answered.
//
// THE OUTER ANSWER WINS, which is what makes this safe to call on every tool
// invocation: a surface reached through a credential that resolved to
// somebody — an operator's own session or token — has a principal already,
// and overwriting it with the seat would attribute their gesture to the agent.
//
// ANY answer, and that includes [iam.Anonymous]: a context a resolver checked
// and found nobody on is a finding this function must not overrule, or every
// request that presented nothing would be decided as whichever turn it
// happened to carry. It is also why the sandbox bridge does NOT rely on this:
// its route is exempt from the credential guard, so the guard answers
// anonymous for every box, and the bridge replaces that answer with the run's
// seat itself — see internal/api/mcpbridge's Session.actingAs. A surface whose
// credential IS the run states the seat outright; this function only fills a
// context nobody has answered for.
func WithPrincipal(ctx context.Context, t *Turn) context.Context {
	if _, how := iam.From(ctx); how != iam.Unknown {
		return ctx
	}
	held := Principal(t)
	if held.Seat == "" {
		// NO SEAT IS NOT NOBODY. A surface outside a turn is a context
		// nothing has resolved, which is exactly what UNKNOWN means —
		// and saying ANONYMOUS here would claim this frame checked.
		return ctx
	}
	return iam.WithPrincipal(ctx, held)
}
