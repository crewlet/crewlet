package engine

import (
	"context"

	"time"

	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// THE FLEET SINGLETONS, and the one gate they all pass through.
//
// A duty here is company-wide work exactly one node does at a time: the
// scheduler tick, the retention sweep, the sandbox waiter's poll, the two
// learning passes. The `worker:{duty}` lease is what makes it one node.
//
// The lease alone is not the whole answer, because it decides WHICH node
// among the ones asking — and `node.roles` is how an operator says which
// nodes may ask. Both halves are needed: without the lease two willing
// nodes both run; without the role check a node explicitly told to run
// nothing but seats still competes for every duty, and wins some of them.
//
// The second half was the missing one. placement.RoleWorkers was declared,
// validated, written into the node's lease meta and rendered by the fleet
// view, and NOTHING consulted it — so `roles: [seats]` claimed the sweep,
// the waiter and the curator anyway, and the package's own promise that "a
// fleet with none of these does none of them" was not kept.

// workerDuty gates a fleet singleton on this node's declared roles.
//
// Three-way, and the difference between the last two matters:
//
//   - NOT A WORKER — refuse, always. Returning nil here would mean "no
//     fleet", which every caller reads as "always mine", i.e. the exact
//     opposite of what the operator wrote.
//   - A WORKER WITH NO COORDINATION STORE — nil, the single-node case:
//     there is nobody to be a singleton among, and a wrapper that always
//     said yes would make a lone node report itself as a fleet member.
//   - A WORKER IN A FLEET — the real lease claim.
func (e *Engine) workerDuty(name string, ttl time.Duration) schedule.DutyFunc {
	if !e.profile.RunsWorkers() {
		return refuseDuty
	}
	if e.backends == nil || e.node == nil {
		return nil
	}
	return schedule.ClaimNamedDuty(e.backends.Coord, name,
		e.node.Owner(), e.node.ID(), ttl)
}

// refuseDuty is the answer for a node whose roles exclude worker duties.
//
// A plain false rather than an error: the node is doing exactly what it was
// configured to do, so every tick logging a failure would be noise on a
// healthy node — and the pass's own "skipped this tick" path is already the
// right behaviour.
func refuseDuty(context.Context) (bool, error) { return false, nil }

// nodeRoles resolves the operator's role names, defaulting to every role.
//
// THE EMPTY SET MEANS EVERY ROLE, which is what makes a single-node
// deployment work with no `node:` block at all — see placement.RoleSet.
func nodeRoles(names []string) placement.RoleSet {
	if len(names) == 0 {
		return placement.DefaultRoles()
	}
	roles := make([]placement.NodeRole, 0, len(names))
	for _, name := range names {
		roles = append(roles, placement.NodeRole(name))
	}
	return placement.Roles(roles...)
}
