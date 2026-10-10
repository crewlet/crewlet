package engine

import (
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// authReading fills the `auth_diverged` alarm's input: the live nodes serving
// the API grouped by the key table they hold, where two groups definitively
// disagree ([placement.DivergentAuth]).
//
// A FLEET-WIDE FACT, evaluated on every node that evaluates the table, so it
// reaches whichever node's health card and log an operator is reading — a data
// node that serves no API included, since it sees the same presence rows.
func (e *Engine) authReading(out *statelog.Reading) {
	out.AuthDiverged = authDivergence(e.dataView, e.profile)
}

// authDivergence is [Engine.authReading] over a presence view and this node's
// own profile.
//
// THE THRESHOLD IS PRESENCE'S (ADR-0015). The view answers the live presence
// leases and nothing else, so a node counts while its lease is live and stops
// counting when it lapses — the lease TTL, the same moment placement stops
// dividing seats by it. The alarm has no clock of its own, and a node that has
// gone never holds it up.
//
// THIS NODE AS IT IS, never its own row: for a lease TTL after a restart the
// row is still the previous process's, whose table may be the very one the
// restart replaced.
//
// A VIEW THAT CANNOT SAY leaves the field empty — nothing measured, which the
// table reads as nothing to report — because a presence listing nobody can
// read shows no table differing, and the store being unreachable is
// `floor_unknown`'s to say. No view at all is a node with no coordination and
// so no fleet: nobody to differ from.
func authDivergence(view *coord.LeaseView, self placement.NodeProfile) [][]string {
	if view == nil {
		return nil
	}
	leases, _, err := view.Leases()
	if err != nil {
		return nil
	}
	nodes := make([]placement.NodeProfile, 0, len(leases)+1)
	for _, lease := range leases {
		profile, ok := placement.FromLease(lease)
		if !ok || profile.ID == self.ID {
			continue
		}
		nodes = append(nodes, profile)
	}
	return placement.DivergentAuth(append(nodes, self))
}
