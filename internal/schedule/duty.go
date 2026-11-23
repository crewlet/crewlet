package schedule

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// The fleet-singleton duty.
//
// Without a singleton every node walks the org and all but one lose the ledger
// claim on every due fire: correct (the claim makes a fire at-most-once) but N
// walks and N claim round trips per tick for one dispatch.
//
// It is a CLAIM PER TICK, not a leader election: no term, quorum, failure
// detector or step-down. A node that dies releases the duty by letting its lease
// lapse, and the next node to tick takes it. A node that stops winning it stops
// advancing its window ([Scheduler.Tick]); on winning again, catchup evaluates
// what it skipped, and the ledger absorbs what the previous holder fired.

// dutyTTLTicks is how many missed ticks the duty survives.
//
// Three, the same "do not flap on a blip" rule the seat heartbeat follows: the
// holder re-claims every tick, so a TTL of three ticks rides out two
// consecutive slow or failed claims without the duty moving to a peer. Moving
// it is not dangerous, but it is churn on a lease the whole fleet reads.
const dutyTTLTicks = 3

// dutyTTLFloor is the shortest duty TTL, whatever the tick interval.
//
// A very short tick would mint a very short lease and re-claim it constantly:
// store traffic for nothing, since the duty works as well at 30 s as at 3 s.
//
// The ceiling on the same value matters more and cannot be enforced here: when
// the holder dies the duty is unclaimable for up to one TTL, and a schedule
// whose period is shorter can lose a fire (the successor's catchup replays only
// the most recent missed fire). At the default 10 s tick the TTL is 30 s, half a
// cron minute, so a dead holder costs one delayed fire that catchup makes up. A
// tick raised toward MaxTick already accepts minute-scale latency, and the TTL
// scales with it.
const dutyTTLFloor = 30 * time.Second

// DutyTTL is the lease TTL a duty claim is made with, for a given tick
// interval. Exported because an operator reading a lease table needs to know
// what a live `worker:scheduler` record means.
func DutyTTL(tick time.Duration) time.Duration {
	return max(dutyTTLTicks*durOr(tick, DefaultTick), dutyTTLFloor)
}

// ClaimDuty builds a [DutyFunc] over a coordination backend.
//
// owner is this process INCARNATION, not the machine or node id: two processes
// sharing an owner string would both hold the duty at the same epoch, the one
// way to get two schedulers. nodeID is the stickiness hint, so a restarted node
// tends to get its duty back.
//
// UNGATED against the protocol floor ([coord.AcquireOptions.Ungated]), which
// guards what holding a SEAT means, so a singleton survives a rolling upgrade.
// It skips the check, never the stamp: the claim carries this build's protocol,
// which holds nothing back (the gate counts presence and seats, never a duty,
// [coord.ProtocolGateCounts]). TryAcquire doubles as a renew keeping its epoch.
func ClaimDuty(backend coord.Backend, owner, nodeID string, tick time.Duration) DutyFunc {
	return ClaimNamedDuty(backend, DutyName, owner, nodeID, DutyTTL(tick))
}

// HoldFunc claims a duty for ONE piece of work and hands back the release.
//
// held is false when a peer has it. release is non-nil exactly when held is
// true, and calling it is what lets the next caller in.
type HoldFunc func(ctx context.Context) (release func(), held bool, err error)

// HoldNamedDuty is mutual exclusion around one piece of work, where
// [ClaimNamedDuty] is a fleet SINGLETON.
//
// The difference is what the TTL is for. A singleton re-claims every tick, so a
// TTL outliving one tick is the point. This locks work with an end (a
// provisioning pass at a third-party app), where the TTL only backstops a node
// that died mid-pass; holding it afterwards locks out every other node.
//
// RELEASED ON A CONTEXT THAT CANNOT BE CANCELLED: a pass whose context died is
// when the lease most needs giving back. The release EXPIRES the record rather
// than deleting it ([coord.Backend.Release]): a deleted record restarts the
// epoch counter a zombie from the released tenure still fences with.
func HoldNamedDuty(backend coord.Backend, duty, owner, nodeID string, ttl time.Duration) HoldFunc {
	if backend == nil {
		// No coordination store is the single-node case, which always
		// holds every duty. Nil rather than a function that always says
		// yes, for the reason [ClaimNamedDuty] gives: a caller logs
		// whether it is a fleet singleton, and a wrapper that always
		// agreed would make a lone node report itself as one.
		return nil
	}
	resource := coord.WorkerResource(duty)
	return func(ctx context.Context) (func(), bool, error) {
		lease, _, err := backend.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner:     owner,
			TTL:       ttl,
			Preferred: nodeID,
			Ungated:   true,
		})
		if err != nil {
			// Unknown, passed straight through, for the reason
			// [ClaimNamedDuty] gives: "a peer holds it" and "the store
			// could not be reached" are the same silence and opposite
			// situations.
			return nil, false, err
		}
		if lease == nil {
			return nil, false, nil
		}
		release := func() {
			// The epoch is the fencing token this tenure was granted
			// under; releasing at any other would give away a lease
			// somebody else now holds.
			if _, relErr := backend.Release(
				context.WithoutCancel(ctx), resource, owner, lease.Epoch,
			); relErr != nil {
				log.Warn("duty_not_released", "duty", duty, "error", relErr,
					"detail", "the next caller waits out the lease TTL instead")
			}
		}
		return release, true, nil
	}
}

// ClaimNamedDuty is [ClaimDuty] for any fleet singleton.
//
// The scheduler is not the only one: the launch-node watch (internal/sandbox)
// judges every launch node in the fleet gone for good or not, so N nodes
// running it unclaimed means N judges recording the same runs lost. Both want
// exactly this arrangement, and a second copy of it is how the two come to
// disagree about what a duty record means.
func ClaimNamedDuty(backend coord.Backend, duty, owner, nodeID string, ttl time.Duration) DutyFunc {
	if backend == nil {
		// No coordination store is the single-node case, which always holds
		// the duty. Returning nil rather than a function that always says
		// yes keeps [Options.Duty] honest: the scheduler logs whether it is
		// a fleet singleton, and a wrapper that always returns true would
		// make a single node report itself as one.
		return nil
	}
	resource := coord.WorkerResource(duty)
	return func(ctx context.Context) (bool, error) {
		lease, _, err := backend.TryAcquire(ctx, resource, coord.AcquireOptions{
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
