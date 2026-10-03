package maintenance

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// LifetimeCounters is the slice of the coordination store this job drives: the
// deletion of the token counters an earlier build kept. Declared here, by the
// consumer, like every other seam in this tree; [coord.LifetimeCounters] is the
// contract and every fleet store satisfies it.
type LifetimeCounters interface {
	// RetireLifetimeCounters deletes them, reporting whether there were
	// any. Retiring what is not there is not an error.
	RetireLifetimeCounters(ctx context.Context) (bool, error)
}

// ProtocolFloor is the slice of the lease store this job reads: the lowest
// seat-host protocol among the fleet's live leases. [coord.Backend] satisfies
// it.
type ProtocolFloor interface {
	FleetProtocolFloor(ctx context.Context) (int, bool, error)
}

// RetiredBudgetJobs is the one job that ends the lifetime token counters
// (ADR-0019): a bucket with no age, holding one figure per scope, that no
// build at [coord.WindowedCountersProtocol] or later reads and nothing
// migrates — a lifetime total has no calendar window to be counted in.
//
// # Gated on the fleet, because an older node still charges them
//
// A build before the windowed counters opened that bucket at boot and charges
// it on every round it runs, so deleting it while such a node is live fails
// that node's every charge closed. The gate is the fleet's PROTOCOL FLOOR: no
// live lease below the protocol that windowed the counters means no node that
// charges the old ones — presence leases included, so an older node that holds
// no seat yet still holds the job back. Until then the job has no work and
// costs one lease listing per sweep; afterwards the first tick deletes the
// bucket and every later one finds nothing to retire.
//
// A [Fleet] job: the bucket is the company's, and one node deleting it is the
// whole of the work. A floor that cannot be read is reported rather than read
// as "not yet", for the reason [Job.Gate] gives. Nil on either seam contributes
// nothing, which is a node with no coordination store and so no bucket.
func RetiredBudgetJobs(counters LifetimeCounters, leases ProtocolFloor) []Job {
	if counters == nil || leases == nil {
		return nil
	}
	return []Job{{
		Name: "retired_budget_bucket", Scope: Fleet,
		Gate: func(ctx context.Context) (bool, error) {
			floor, live, err := leases.FleetProtocolFloor(ctx)
			if err != nil {
				return false, err
			}
			// NO LIVE LEASE AT ALL is not evidence either way. The duty
			// this job runs under is itself a lease, so a store that
			// sees nothing live is one that cannot see this node — and
			// it cannot see an older one either, while a deletion
			// cannot be taken back. Waiting a tick costs nothing.
			return live && floor >= coord.WindowedCountersProtocol, nil
		},
		Run: func(ctx context.Context, _, _ time.Time) (int64, error) {
			retired, err := counters.RetireLifetimeCounters(ctx)
			if retired {
				return 1, err
			}
			return 0, err
		},
	}}
}
