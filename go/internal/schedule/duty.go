package schedule

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// The fleet-singleton duty.
//
// Every node in a fleet runs the same scheduler against the same org, so
// without a singleton every node enumerates every schedule and all but one
// lose the ledger claim on every due fire. That is not INCORRECT — the claim
// is what makes a fire at-most-once, and it holds under any number of racers —
// it is pure duplicated work: N walks of the org and N claim round trips per
// tick to produce one dispatch.
//
// It is a CLAIM PER TICK, not a leader election, and the difference is the
// point. A claim needs no term, no quorum, no failure detector and no
// step-down protocol: a node that dies between ticks releases the duty by
// letting its lease lapse, and the next node to tick takes it. Nothing has to
// notice the death, because nothing was waiting for the dead node to say
// anything.
//
// Losing the duty is also harmless in a way a lost leadership is not. A node
// that stops winning it stops advancing its own window (see [Scheduler.Tick]),
// so it stays on its first tick; when it does win one, the catchup pass
// evaluates what it skipped rather than a window stretching back to boot. The
// ledger absorbs whatever the previous holder already fired.

// dutyTTLTicks is how many missed ticks the duty survives.
//
// Three, the same "do not flap on a blip" rule the seat heartbeat follows: the
// holder re-claims every tick, so a TTL of three ticks rides out two
// consecutive slow or failed claims without the duty moving to a peer. Moving
// it is not dangerous, but it is churn on a lease the whole fleet reads.
const dutyTTLTicks = 3

// dutyTTLFloor is the shortest duty TTL, whatever the tick interval.
//
// A very short tick would otherwise mint a very short lease and re-claim it
// constantly, which is store traffic bought for nothing: the duty's job is to
// stop N nodes doing one node's work, and it does that just as well at 30 s as
// at 3 s.
//
// The ceiling on the same value is the number that actually matters, and it is
// not enforced here because it cannot be: when the holder dies, the duty is
// unclaimable for up to one TTL, and a schedule whose period is shorter than
// that can lose a fire (the successor's catchup pass replays the most recent
// missed fire, not every one). At the default 10 s tick the TTL is 30 s — half
// a cron minute, so a dead holder costs at most one delayed fire, which
// catchup then makes up. An operator who raises the tick toward MaxTick is
// already accepting minute-scale dispatch latency, and the TTL scales with
// that choice rather than fighting it.
const dutyTTLFloor = 30 * time.Second

// DutyTTL is the lease TTL a duty claim is made with, for a given tick
// interval. Exported because an operator reading a lease table needs to know
// what a live `worker:scheduler` record means.
func DutyTTL(tick time.Duration) time.Duration {
	return max(dutyTTLTicks*durOr(tick, DefaultTick), dutyTTLFloor)
}

// ClaimDuty builds a [DutyFunc] over a coordination backend.
//
// owner is this process INCARNATION — not the machine, and not the node id.
// Two processes sharing an owner string would both believe they hold the duty
// at the same epoch, which is the one way this arrangement can produce two
// schedulers. nodeID is the stable id, recorded as the stickiness hint so a
// restarted node tends to get its own duty back.
//
// The claim is UNGATED against the protocol floor, deliberately, and for the
// reason coord's own doc gives: a duty record left at an older protocol by a
// build that predates the gate would block every seat claim fleet-wide the
// moment the version moved. A duty claim carries this build's protocol, so it
// never becomes the thing that blocks.
//
// TryAcquire doubles as a renew for the current owner and keeps its epoch, so
// the per-tick claim is one store round trip and the holder stays the holder
// while it keeps ticking.
func ClaimDuty(backend coord.Backend, owner, nodeID string, tick time.Duration) DutyFunc {
	if backend == nil {
		// No coordination store is the single-node case, which always holds
		// the duty. Returning nil rather than a function that always says
		// yes keeps [Options.Duty] honest: the scheduler logs whether it is
		// a fleet singleton, and a wrapper that always returns true would
		// make a single node report itself as one.
		return nil
	}
	ttl := DutyTTL(tick)
	resource := coord.WorkerResource(DutyName)
	return func(ctx context.Context) (bool, error) {
		lease, err := backend.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner:     owner,
			TTL:       ttl,
			Preferred: nodeID,
			Ungated:   true,
		})
		if err != nil {
			// Unknown, passed straight through. The scheduler fails closed
			// on it; collapsing it into a false here would hide from the
			// log the difference between "a peer holds it" and "the store
			// could not be reached", which are the same silence and very
			// different situations.
			return false, err
		}
		return lease != nil, nil
	}
}
