package engine

import (
	"context"
	"slices"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// WithheldBuiltins is the builtins of snapshot that seat, an agent seat of the
// chart o, is offered on no surface of its turns, sorted: those the authority
// table refuses it before any object is named (builtin.Withheld) — a verb whose
// row names a role grant its role does not carry — decided over o as the seat
// (turnctx.SeatPrincipal), so what a seat is offered and what each of its calls
// is decided as are one principal; and the code writes no grant at the seat's
// code tier admits (builtin.CodeWritesWithheld) — a refusal every repository's
// grant makes, which the table, admitting everybody to a code write so each
// repository's grant decides it, cannot make ahead of the calls. The tier is
// read off seat, as Gitlet's roster reads it (codeRoster). Nil for a nil seat.
//
// THE ONE ANSWER TO WHAT A SEAT IS NOT OFFERED, for both its readers: a turn's
// surfaces, cut by it over the epoch the turn is pinned to
// ([Company.withheldFrom]), and the count Settings › MCP & tools reads of the
// built-ins each agent is offered (`mcp_servers_status`' `builtin_tools`,
// through internal/api's runtime), decided over the chart that answer lists the
// agent in. Two readings of one question part the first time one of them gains
// a layer the other lacks, and the page would count an agent tools its turns
// are never offered; TestAnAgentIsCountedTheBuiltinsItsTurnIsShown holds them
// to each other.
//
// A SNAPSHOT, not a registry, so a caller that listed rows off one is answered
// in the same names.
func WithheldBuiltins(ctx context.Context, o *org.Organization, seat *org.Role,
	snapshot tools.Snapshot,
) []string {
	if seat == nil {
		return nil
	}
	ctx = iam.WithPrincipal(ctx, turnctx.SeatPrincipal(o, seat))
	withheld := builtin.Withheld(ctx, builtin.Decide(orgchart.Of(o)), snapshot)
	for _, name := range builtin.CodeWritesWithheld(gitlet.Tier(seat.CodeAccess)) {
		if entry, ok := snapshot.Lookup(name); ok && entry.Origin == tools.OriginBuiltin &&
			!slices.Contains(withheld, name) {
			withheld = append(withheld, name)
		}
	}
	slices.Sort(withheld)
	return withheld
}

// withheldFrom is [WithheldBuiltins] for handle's turn: decided over THIS
// EPOCH's chart, the one the turn is pinned to and builds its prompt from,
// against reg, the registry its surfaces are built from.
func (c *Company) withheldFrom(ctx context.Context, handle string, reg *tools.Registry) []string {
	if reg == nil {
		return nil
	}
	return WithheldBuiltins(ctx, c.Org, c.Org.AgentSeatByHandle(handle), reg.Snapshot())
}
