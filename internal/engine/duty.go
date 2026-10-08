package engine

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/seat"
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
	e.duties.add(name)
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
	// No lease store is [schedule.HoldNamedDuty]'s own nil branch, asked
	// here because the store it is handed below is never nil.
	if e.backends == nil || e.backends.Coord == nil || e.node == nil {
		return nil
	}
	return schedule.HoldNamedDuty(holdLeases{Backend: e.backends.Coord, engine: e}, name,
		e.node.Owner(), e.node.ID(), ttl)
}

// holdLeases is the lease store this node's holds are taken through: its own,
// with every give-back made once its stop has begun drawn from the stop's one
// allowance ([Engine.stopping]).
//
// A HOLD IS GIVEN BACK BY WHOEVER ENDS THE WORK IT GUARDS — the setup runner,
// at the end of a pass the integration loop or an operator ran — on the context
// the hold was TAKEN on with its cancellation removed ([schedule.HoldNamedDuty]),
// so nothing of a stop reaches it. The teardown cancels the integration loop
// and waits for the pass in flight, and that pass gave its lease back on a
// context with no deadline at all: against a store that has gone away, one
// client request timeout the stop sat out beside its allowance — measured at
// five seconds past it — on a give-back whose fallback is the lapse on the TTL
// every other step of the stop falls back to. Bounded HERE because this is the
// one frame that holds both the store a hold is taken through and the stop that
// can end it; the hold's own context was made before the stop existed.
//
// ONLY ONCE THE STOP HAS BEGUN. A pass ending on its own is not a step of
// anybody's stop, and charging the allowance for it would spend, during an
// ordinary day, the time the stop is owed. A give-back already in flight when
// the stop begins keeps the bound it was made under, like every round trip in
// flight at that moment.
type holdLeases struct {
	coord.Backend
	engine *Engine
}

// Release is [coord.Backend.Release], one step of the stop once it has begun.
func (l holdLeases) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	ctx, done := seat.StopStep(seat.WithStopBudget(ctx, l.engine.stopBudget.Load()))
	defer done()
	return l.Backend.Release(ctx, resource, owner, epoch)
}

// refuseDuty is the answer for a node whose roles exclude worker duties.
//
// A plain false rather than an error: the node is doing exactly what it was
// configured to do, so every tick logging a failure would be noise on a
// healthy node — and the pass's own "skipped this tick" path is already the
// right behaviour.
func refuseDuty(context.Context) (bool, error) { return false, nil }

// claimedDuties is the set of duty names an engine claims through
// [Engine.workerDuty], recorded there so the list [Engine.releaseDuties] walks
// cannot drift from the duties that actually run.
type claimedDuties struct {
	mu    sync.Mutex
	names []string
}

func (d *claimedDuties) add(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !slices.Contains(d.names, name) {
		d.names = append(d.names, name)
	}
}

func (d *claimedDuties) list() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.names)
}

// releaseDuties gives back every fleet duty this incarnation still holds.
//
// # Why a duty is released on a graceful stop
//
// A duty is claimed per tick and never released by its loop, so a node that
// stops holds it until the lease lapses, and the lease is sized to outlive
// several ticks: 45 minutes for the retention sweep, three hours for the skill
// curator. A restart takes a new incarnation, which cannot re-claim what the
// old one held, so every deploy that restarted the holder left the duty dark
// for its whole TTL, and a fleet deployed more often than every three hours
// could starve the curator altogether. A seat is given back on a drain for the
// same reason; a duty is cheaper still to give back, because nothing is
// attached to it.
//
// ONLY WHAT [Engine.workerDuty] CLAIMED. A hold ([Engine.workerHold]) wraps one
// piece of work that is released when the work ends, and may belong to a pass
// the API is still running on the merged topology, where the API outlives the
// engine: releasing it here would let a second writer at a third-party app in
// mid-pass, which is the collision the hold exists to prevent.
//
// Called after every duty loop has stopped, and each loop's Stop waits for a
// tick in flight, so no tick of this node runs once a peer can take the duty.
// Released at the epoch the store reports for this owner, so a lease a peer
// has since taken is never touched.
//
// ONE STEP OF THE STOP'S ALLOWANCE ([seat.StopStep]), which the teardown
// always carries: giving a lease back is a handful of coordination writes,
// past the allowance a duty lapses on its TTL, which is the outcome of not
// trying — and a bound of its own here was one more bound a member without
// quorum spent in full, after every step before it had spent theirs.
func (e *Engine) releaseDuties(ctx context.Context) {
	if e.backends == nil || e.backends.Coord == nil || e.node == nil {
		return
	}
	ctx, done := seat.StopStep(context.WithoutCancel(ctx))
	defer done()
	owner := e.node.Owner()
	for _, name := range e.duties.list() {
		resource := coord.WorkerResource(name)
		lease, err := e.backends.Coord.Get(ctx, resource)
		if err != nil {
			log.WarnContext(ctx, "duty_not_released", "duty", name, "error", err,
				"detail", "the duty could not be read, so a peer takes it over once its lease lapses")
			continue
		}
		if lease == nil || lease.Owner != owner {
			continue
		}
		if _, err := e.backends.Coord.Release(ctx, resource, owner, lease.Epoch); err != nil {
			log.WarnContext(ctx, "duty_not_released", "duty", name, "error", err,
				"detail", "a peer takes the duty over once its lease lapses")
			continue
		}
		log.InfoContext(ctx, "duty_released", "duty", name)
	}
}
