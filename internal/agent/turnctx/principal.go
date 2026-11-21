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
// THE SEAT'S HANDLE IS THE IDENTITY, kind [iam.KindSeat] ([iam.ActorFor] makes
// an `agent` author of it). A turn with no seat answers the zero principal,
// refused by every class: admitting it would make "no acting seat" the most
// powerful caller.
//
// EVERY APP, not per seat: the ROLE bounds a seat (no knowledge tools, no page
// writes), so a permission agreeing with the registry would be a second answer.
// NEVER ADMIN: an agent rewriting its company or reading every credential and
// prompt is the blast radius the vocabulary bounds. The seat-only verb
// (`a2a_ask`) is the table's `seatOnly` row. Grants are a CLONE (no shared list).
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

// WithPrincipal attaches the turn's seat as the acting party, unless something
// upstream has already answered. THE OUTER ANSWER WINS, which makes this safe on
// every tool invocation: overwriting an operator's own principal would
// attribute their gesture to the agent.
//
// ANY answer, [iam.Anonymous] included: a context a resolver checked and found
// nobody on must not be overruled, or a request presenting nothing would be
// decided as whichever turn it carried. So the sandbox bridge does NOT rely on
// this: its route is exempt from the credential guard, which answers anonymous
// for every box, and the bridge states the run's seat itself (internal/api/
// mcpbridge's Session.actingAs).
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
