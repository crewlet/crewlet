package engine

import "github.com/crewlet/crewlet/internal/agent/builtin"

// HealthToolDeps are the health tools' dependencies on this node — the fleet's
// presence leases, the requester ceiling's halves ([turnRequesters],
// [ChartAuthorityOf]) and, where this node serves Gitlet, the status scatter
// ([Engine.CodeStatus]) with the code tools' deps — or the zero value (which
// serves neither) on a node with no coordination store.
//
// fleet_health is offered on every node, in a turn woken only for someone of
// the company who may act; code_status exactly where the code tools are
// ([CodeToolDeps]). Exported for the operator surface, which builds its
// catalogue per call; a seat's registry takes it at every apply
// ([Engine.equip]). The ceiling's halves are read per call, for [CodeToolDeps]'
// reason: a held chart outlives the activation that set it.
func HealthToolDeps(e *Engine) builtin.HealthDeps {
	if e == nil || e.backends == nil || e.backends.Coord == nil {
		return builtin.HealthDeps{}
	}
	deps := builtin.HealthDeps{Leases: e.backends.Coord, Self: e.id,
		Requesters: turnRequesters{engine: e}, Chart: ChartAuthorityOf(e)}
	if reads := CodeToolDeps(e); reads.Code != nil {
		deps.Status, deps.Code = e, reads
	}
	return deps
}
