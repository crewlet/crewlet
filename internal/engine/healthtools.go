package engine

import (
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
)

// THE HEALTH TOOLS' ENGINE WIRING: the same deps for a seat's registry and for
// the operator catalogue — the fleet's presence leases, which every node reads,
// with whoever a turn is acting for and the chart that decides them (the
// requester ceiling's halves, the code and artifacts tools' own:
// [turnRequesters] and [ChartAuthorityOf]), and the Gitlet nodes' status scatter
// ([Engine.CodeStatus]), with the code tools' own deps beside it where this
// node's running runtime serves Gitlet.

// HealthToolDeps are the health tools' dependencies on this node, or the zero
// value (which serves neither) on a node with no coordination store.
//
// fleet_health WHEREVER THE LEASES ARE READ, every node of a fleet, read in a
// turn somebody woke only for somebody of the company who may act; code_status
// ON [CodeToolDeps]' TERMS, so a seat's and a person's code_status is offered
// exactly where the code tools are — none for a company that hosts no code,
// whose fleet has no Gitlet node to ask.
//
// EXPORTED for the operator surface, which builds its catalogue for every
// call; a seat's registry takes the value at every apply ([Engine.equip]).
// The ceiling's halves are read per call, for [CodeToolDeps]' reason: a value
// holding this epoch's chart would judge a requester by an organisation a
// later activation replaced.
func HealthToolDeps(e *Engine) builtin.HealthDeps {
	if e == nil || e.backends == nil || e.backends.Coord == nil {
		return builtin.HealthDeps{}
	}
	deps := builtin.HealthDeps{Leases: e.backends.Coord, Self: e.id,
		Requesters: turnRequesters{engine: e}, Chart: ChartAuthorityOf(e),
		Now: func() time.Time { return time.Now().UTC() }}
	if reads := CodeToolDeps(e); reads.Code != nil {
		deps.Status, deps.Code = e, reads
	}
	return deps
}
