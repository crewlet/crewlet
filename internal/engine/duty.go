package engine

import (
	"context"

	"time"

	"github.com/crewlet/crewlet/internal/schedule"
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

// workerHold is [Engine.workerDuty] for a lease that is GIVEN BACK.
//
// # A hold is NOT a duty, and the difference is the roles gate
//
// A duty is company-wide work exactly one node does at a time, so refusing it
// on `node.roles` is the operator getting what they asked for: they said this
// node runs no workers, and a node that ran them anyway would be ignoring the
// config.
//
// A hold is mutual exclusion around work THIS NODE HAS ALREADY BEEN ASKED TO
// DO — provisioning an integration somebody pressed Connect on, or a disconnect
// they pressed Disconnect on, at whichever node happens to be serving the API.
// Refusing it does not decline the work; it makes the work impossible while
// looking exactly like a peer already doing it.
//
// It DID refuse, because this shared the gate above, and `-roles ingress` is
// the documented split that puts the dashboard on a node with no worker role.
// There, `refuseHold` was handed to [setup.Runner] as a Duty that answers
// not-held for ever — non-nil, so the "no coordination store" branch never
// caught it — and every Connect answered 409 "another pass for this integration
// is running; wait for it rather than minting twice" over a surface where
// nothing was running, every Recheck the same, and every Disconnect 503 "being
// provisioned right now; try again in a moment". Permanently, on the only node
// serving the screen that offers those buttons.
//
// So this gates on the coordination store alone. What differs from a duty is
// also the shape of the lease underneath: a singleton is re-claimed every tick
// and the holder stays the holder, so its TTL is meant to outlive one tick,
// while a hold wraps one piece of work and is released when that work ends —
// keeping it afterwards is indistinguishable from an outage to every other
// caller. See [schedule.HoldNamedDuty].
func (e *Engine) workerHold(name string, ttl time.Duration) schedule.HoldFunc {
	if e.backends == nil || e.node == nil {
		return nil
	}
	return schedule.HoldNamedDuty(e.backends.Coord, name,
		e.node.Owner(), e.node.ID(), ttl)
}

// refuseDuty is the answer for a node whose roles exclude worker duties.
//
// A plain false rather than an error: the node is doing exactly what it was
// configured to do, so every tick logging a failure would be noise on a
// healthy node — and the pass's own "skipped this tick" path is already the
// right behaviour.
func refuseDuty(context.Context) (bool, error) { return false, nil }
