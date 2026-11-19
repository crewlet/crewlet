package engine

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/seat"
)

// THE FLEET SINGLETONS: company-wide work exactly one node does at a time,
// every caller of [Engine.workerDuty], named in architecture.md's and
// seat-ownership.md's duty tables (dutydocs_internal_test.go holds them to the
// callers). The `worker:{duty}` lease decides WHICH node among those asking;
// `node.roles` decides which nodes may ask. Both are needed: without the lease
// two willing nodes both run, and without the role check a `roles: [seats]`
// node competes for every duty.

// workerDuty gates a fleet singleton on this node's declared roles:
//
//   - NOT A WORKER: refuse. A nil would mean "no fleet", which every caller
//     reads as "always mine", the opposite of what the operator wrote.
//   - A WORKER WITH NO COORDINATION STORE: nil, the single-node case; a
//     wrapper that always said yes would report a lone node as a fleet member.
//   - A WORKER IN A FLEET: the real lease claim.
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

// liveDuty is a fleet singleton held where the live projection is seeded, not
// where `node.roles` says: the flow duty reads which items a turn is on and
// which seats are stopped from it, and only a node serving the API folds it
// (ADR-0162). Read without it, a stopped seat's work reads due or stuck, so a
// worker node that held the duty would tell people wrong things; a fleet whose
// API and workers run on different nodes still runs it, on an API node.
//
//   - NO COORDINATION STORE: a lone node, holding it whenever it is live.
//   - IN A FLEET: the lease, asked for only while live, and given back the
//     tick it stops being live, so a live peer takes it at its next tick
//     rather than after the TTL.
func (e *Engine) liveDuty(name string, ttl time.Duration) liveLease {
	if e.backends == nil || e.backends.Coord == nil || e.node == nil {
		return liveLease{}
	}
	e.duties.add(name)
	return newLiveLease(e.backends.Coord, name, e.node.Owner(), e.node.ID(), ttl)
}

// liveLease is [Engine.liveDuty]'s claim and give-back; the zero value is a
// lone node's, which holds the duty whenever it is live.
type liveLease struct {
	claim   schedule.DutyFunc
	release func(context.Context) (bool, error)

	// held is whether the last answer was held, so a node that stops being
	// live gives back only a lease it took.
	held bool
}

// newLiveLease is the lease on name for owner, preferring nodeID.
func newLiveLease(backend coord.Backend, name, owner, nodeID string, ttl time.Duration) liveLease {
	resource := coord.WorkerResource(name)
	return liveLease{
		claim: schedule.ClaimNamedDuty(backend, name, owner, nodeID, ttl),
		release: func(ctx context.Context) (bool, error) {
			lease, err := backend.Get(ctx, resource)
			if err != nil || lease == nil || lease.Owner != owner {
				return false, err
			}
			return backend.Release(ctx, resource, owner, lease.Epoch)
		},
	}
}

// holds reports whether this node holds the duty this tick: never while not
// live, giving back a lease it held (retried each tick until the store
// answers), and by its lease while live.
func (l *liveLease) holds(ctx context.Context, live bool) (bool, error) {
	if !live {
		if l.held && l.release != nil {
			if _, err := l.release(ctx); err != nil {
				return false, err
			}
		}
		l.held = false
		return false, nil
	}
	held := true
	if l.claim != nil {
		var err error
		if held, err = l.claim(ctx); err != nil {
			return false, err
		}
	}
	l.held = held
	return held, nil
}

// workerHold is [Engine.workerDuty] for a lease that is GIVEN BACK, gated on
// the coordination store alone, never on `node.roles`.
//
// A hold guards work THIS NODE WAS ALREADY ASKED TO DO (a Connect pressed at
// whichever node serves the API): refusing it makes the work impossible while
// looking like a peer doing it, forever on an `-roles ingress` dashboard node.
// It is released when its work ends ([schedule.HoldNamedDuty]).
//
// UNDER THIS PROCESS'S INCARNATION, not the node's lease owner: a hold may be
// taken before the node exists (the identity estate mints the blind-index key
// from boot), and a fleet booting at once is exactly when two members mint.
func (e *Engine) workerHold(name string, ttl time.Duration) schedule.HoldFunc {
	// The store handed below is never nil, so HoldNamedDuty's nil branch is
	// asked here. The node is not: a hold may precede it.
	if e.backends == nil || e.backends.Coord == nil {
		return nil
	}
	return schedule.HoldNamedDuty(holdLeases{Backend: e.backends.Coord, engine: e}, name,
		e.incarnation, e.id, ttl)
}

// holdLeases is the lease store this node's holds and the sandbox
// coordinator's taker lease ([Engine.coordinatorOptions]) are taken through,
// each give-back drawn from the stop's one allowance once the TEARDOWN has
// begun ([Engine.stepOnceTornDown]); the teardown stops the coordinator
// ([Engine.stopSandbox]) and waits for any pass in flight.
//
// BOUNDED HERE because a hold is given back on its taking context with the
// cancellation removed ([schedule.HoldNamedDuty]), made before the stop
// existed: unbounded, a gone store cost a client timeout past the allowance.
// ONLY DURING THE TEARDOWN: a give-back during the unbounded DRAIN is nobody's
// stop step, and charging it would spend the time the seats' hand-off is owed.
type holdLeases struct {
	coord.Backend
	engine *Engine
}

// Release is [coord.Backend.Release], a step of the stop for as long as it
// runs once the teardown has begun.
func (l holdLeases) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	ctx, done := l.engine.stepOnceTornDown(ctx)
	defer done()
	return l.Backend.Release(ctx, resource, owner, epoch)
}

// refuseDuty is the answer for a node whose roles exclude worker duties: a
// plain false rather than an error, since the node is doing what it was
// configured to, and a failure logged every tick would be noise.
func refuseDuty(context.Context) (bool, error) { return false, nil }

// claimedDuties is the duty names claimed through [Engine.workerDuty],
// recorded there so [Engine.releaseDuties] cannot drift from what runs.
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

// releaseDuties gives back every fleet duty this incarnation still holds: a
// duty's lease outlives several ticks (three hours for the skill curator), and
// a restarted node's new incarnation cannot re-claim it, so every deploy would
// leave the duty dark for its whole TTL.
//
// ONLY WHAT [Engine.workerDuty] CLAIMED: releasing a hold ([Engine.workerHold])
// would let a second writer into a pass the API, outliving the engine, runs.
//
// Called after every duty loop's Stop has waited for its tick in flight, so no
// tick runs once a peer can take the duty. ONE STEP OF THE STOP'S ALLOWANCE
// ([seat.StopStep]), not a bound of its own: past it a duty lapses on its TTL,
// and a bound of its own was one more a member without quorum spent in full.
func (e *Engine) releaseDuties(ctx context.Context) {
	if e.backends == nil || e.backends.Coord == nil || e.node == nil {
		return
	}
	ctx, done := seat.StopStep(context.WithoutCancel(ctx))
	defer done()
	for _, name := range e.duties.list() {
		released, err := e.releaseDuty(ctx, name)
		switch {
		case err != nil:
			log.WarnContext(ctx, "duty_not_released", "duty", name, "error", err,
				"detail", "a peer takes the duty over once its lease lapses")
		case released:
			log.InfoContext(ctx, "duty_released", "duty", name)
		}
	}
}

// releaseDuty gives back the fleet duty name if this incarnation holds it, and
// reports whether it did. Released at the epoch the store reports for this
// owner, so a lease a peer has since taken is never touched.
func (e *Engine) releaseDuty(ctx context.Context, name string) (bool, error) {
	owner := e.node.Owner()
	resource := coord.WorkerResource(name)
	lease, err := e.backends.Coord.Get(ctx, resource)
	if err != nil {
		return false, fmt.Errorf("read the %s duty's lease: %w", name, err)
	}
	if lease == nil || lease.Owner != owner {
		return false, nil
	}
	released, err := e.backends.Coord.Release(ctx, resource, owner, lease.Epoch)
	if err != nil {
		return false, fmt.Errorf("give the %s duty's lease back: %w", name, err)
	}
	return released, nil
}

// workerGiveBack is the give-back of the fleet duty [Engine.workerDuty] claims
// under name, for a holder that DECLINES it while still up ([dutyDecline]),
// reporting whether this node held it and gave it back. Given back rather
// than left to lapse, since a lease outlives several ticks (the trim's 45
// minutes plus a tick), so the next node that does not decline takes it at its
// next tick. Nil wherever workerDuty claims no lease.
func (e *Engine) workerGiveBack(name string) func(context.Context) (bool, error) {
	if !e.profile.RunsWorkers() || e.backends == nil || e.backends.Coord == nil || e.node == nil {
		return nil
	}
	return func(ctx context.Context) (bool, error) { return e.releaseDuty(ctx, name) }
}
