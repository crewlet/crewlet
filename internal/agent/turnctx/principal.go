package turnctx

import (
	"context"

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

// SeatGrants are the capabilities an agent seat carries.
//
// FOUR, and the set is the whole of what a seat's tools ask for:
//
//   - `state:read` opens the ordinary reads — the board, the pages, the
//     roster, the catalogue. A seat that could not read them could not do
//     anything at all, which is why this is not configurable per seat: a
//     company that wanted a seat to see nothing would not give it a seat.
//   - `work:write` and `knowledge:write` are the two colleague writes,
//     separate for the reason internal/iam states: a company routinely wants
//     an automation that files bugs and may not edit the handbook, and a seat
//     inherits both because its ROLE is what bounds it — a seat with no
//     knowledge tools registered cannot write a page whatever it carries.
//   - `sandbox:run` is the detached coding run. Carried rather than
//     conditional, because whether a seat CAN run one is whether a sandbox is
//     wired into its deps, and a grant that had to agree with that wiring
//     would be a second answer to one question.
//
// WHAT IS DELIBERATELY ABSENT is every operator capability: a seat holds no
// `config:write`, no `secrets:*`, no `fleet:operate`, no `people:manage` and
// no `audit:read`. An agent that could rewrite the company it runs inside, or
// read every prompt every other agent was given, is the blast radius this
// vocabulary exists to bound — and the one grant a company might reasonably
// want to give a seat is precisely the one it must not be able to take.
var SeatGrants = []iam.Grant{
	iam.GrantStateRead, iam.GrantWorkWrite, iam.GrantKnowledgeWrite,
	iam.GrantSandboxRun,
}

// Principal is who a turn acts as.
//
// THE SEAT'S HANDLE IS THE IDENTITY, and the kind is [iam.KindSeat] — which
// is what the authority table's own-record and colleague classes compare
// against, and what [iam.ActorFor] turns into an `agent` author. A turn with
// no seat answers the zero principal, which holds nothing and is refused by
// every class: a tool surface built outside a turn legitimately has no seat,
// and admitting it would make "no acting seat" the most powerful caller in
// the engine.
func Principal(t *Turn) iam.Principal {
	seat, err := t.RequireSeat()
	if err != nil {
		return iam.Principal{}
	}
	return iam.Principal{
		Kind:  iam.KindSeat,
		Seat:  seat.Handle(),
		Stage: iam.StageActive,
		// EVERY SEAT IS A COLLEAGUE WHO WRITES, which is not a grant and
		// not a rung on the same ladder: a colleague level is reach into
		// the company's own WORK, and a seat that could not be assigned
		// to, mentioned or delegated to is a seat nothing can use.
		Colleague: iam.ColleagueWrite,
		Grants:    SeatGrants,
		Position:  unitOf(t, seat),
	}
}

// unitOf is the unit a seat sits in, or empty.
//
// CARRIED ON THE PRINCIPAL rather than looked up per decision, for
// [iam.Principal.Position]'s own reason: a gate that re-walked the org chart
// per request would read a tree that has moved since the turn began.
func unitOf(t *Turn, seat *org.Role) string {
	if t == nil || t.Org == nil {
		return ""
	}
	unit := t.Org.UnitFor(seat)
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
// somebody — the MCP bridge's per-run token, an operator's own session — has
// a principal already, and overwriting it with the seat would attribute their
// gesture to the agent.
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
