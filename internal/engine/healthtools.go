package engine

import (
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE HEALTH TOOLS' ENGINE WIRING: the same deps for a seat's registry and for
// the operator catalogue — the fleet's presence leases, which every node reads,
// and the git nodes' status scatter ([Engine.CodeStatus]), with the code tools'
// own deps beside it where this node's running runtime serves the code host.

// HealthToolDeps are the health tools' dependencies on this node, or the zero
// value (which serves neither) on a node with no coordination store.
//
// fleet_health WHEREVER THE LEASES ARE READ, every node of a fleet; code_status
// ON [CodeToolDeps]' TERMS, so a seat's and a person's code_status is offered
// exactly where the code tools are — none for a company that hosts no code,
// whose fleet has no git node to ask.
//
// EXPORTED for the operator surface, which builds its catalogue for every
// call; a seat's registry takes the value at every apply ([Engine.equip]).
func HealthToolDeps(e *Engine) builtin.HealthDeps {
	if e == nil || e.backends == nil || e.backends.Coord == nil {
		return builtin.HealthDeps{}
	}
	deps := builtin.HealthDeps{Leases: e.backends.Coord, Self: e.id,
		Now: func() time.Time { return time.Now().UTC() }}
	if reads := CodeToolDeps(e); reads.Code != nil {
		deps.Status, deps.Code = e, reads
	}
	return deps
}

// withheldFrom is the builtins of reg that handle's surfaces must not offer it
// this turn, sorted: those the authority table refuses the seat before any
// object is named (builtin.Withheld) — a verb whose row names a role grant the
// seat's role does not carry — decided over THIS EPOCH's chart, the one the
// turn is pinned to and builds its prompt from, as the seat the turn acts as
// (turnctx.SeatPrincipal), so what the turn is offered and what each of its
// calls is decided as are one principal; and the code writes, where the
// seat's code tier writes no repository at all (builtin.CodeWritesWithheld) —
// a refusal every repository's grant makes, which the table, admitting
// everybody to a code write so each repository's grant decides it, cannot
// make ahead of the calls. The tier is read off the same epoch's role, as the
// code host's roster reads it (codeRoster).
func (c *Company) withheldFrom(ctx context.Context, handle string, reg *tools.Registry) []string {
	role := c.Org.AgentSeatByHandle(handle)
	if role == nil || reg == nil {
		return nil
	}
	ctx = iam.WithPrincipal(ctx, turnctx.SeatPrincipal(c.Org, role))
	withheld := builtin.Withheld(ctx, builtin.Decide(orgchart.Of(c.Org)), reg.Snapshot())
	for _, name := range builtin.CodeWritesWithheld(code.Tier(role.CodeAccess)) {
		if entry, ok := reg.Lookup(name); ok && entry.Origin == tools.OriginBuiltin &&
			!slices.Contains(withheld, name) {
			withheld = append(withheld, name)
		}
	}
	slices.Sort(withheld)
	return withheld
}
