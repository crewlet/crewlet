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

// WithheldBuiltins is the builtins of snapshot that seat, an agent seat of
// chart o, is offered on no surface of its turns, sorted; nil for a nil seat.
// It is what the authority table refuses before any object is named
// (builtin.Withheld), decided as the seat (turnctx.SeatPrincipal) so offer and
// call share one principal, plus the code writes no grant at the seat's code
// tier admits (builtin.CodeWritesWithheld; tier read as codeRoster reads it),
// which the table, leaving each to its repository's grant, cannot refuse
// ahead of the call. The one answer for both readers, a turn's surfaces
// ([Company.withheldFrom]) and the per-agent `builtin_tools` count on
// Settings › MCP & tools, so the page never counts a tool a turn is not
// offered (TestAnAgentIsCountedTheBuiltinsItsTurnIsShown). A snapshot, not a
// registry, so a caller is answered in the names it listed.
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
