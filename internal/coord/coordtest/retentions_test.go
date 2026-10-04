package coordtest_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The retentions have to stay consistent with the cadences they are sized
// from, and both live in packages that cannot import each other. A drift
// would be silent: a status freshness below the reconcile interval makes
// every healthy node look stale, and a ledger retention below the redelivery
// horizon lets a trigger run twice.
func TestTheRetentionsOutlastWhatTheyCover(t *testing.T) {
	t.Parallel()
	if coord.StatusFreshness <= coord.ReconcileInterval {
		t.Errorf("StatusFreshness %v does not outlast one reconcile tick (%v), so a "+
			"node that reported on time still reads as stale",
			coord.StatusFreshness, coord.ReconcileInterval)
	}
	if coord.ReconcileInterval != configplane.ReconcileInterval {
		t.Errorf("coord.ReconcileInterval %v has drifted from the configplane's %v",
			coord.ReconcileInterval, configplane.ReconcileInterval)
	}
	if coord.StatusFreshness != configplane.PeerStatusFreshness {
		t.Errorf("coord.StatusFreshness %v has drifted from the configplane's %v",
			coord.StatusFreshness, configplane.PeerStatusFreshness)
	}
	// The completion ledger's floor is the scheduler's catchup ceiling, not
	// a round number: expiring a record a tick could still evaluate lets
	// that fire run TWICE, which is the one thing the ledger exists to
	// prevent. The bucket's age is now the only thing enforcing it, so a
	// catchup window widened past the retention has to fail here.
	if coord.LedgerRetention <= schedule.DefaultCatchupMax {
		t.Errorf("coord.LedgerRetention %v can expire a completion a tick could still "+
			"evaluate (catchup ceiling %v), so a scheduled fire runs twice",
			coord.LedgerRetention, schedule.DefaultCatchupMax)
	}
	// The fire CLAIM's floor is the same ceiling, and for a sharper reason:
	// a completion that expired early makes a turn re-run, while a claim
	// that expired early makes the catchup pass dispatch a fire the fleet
	// already ran.
	if coord.FireRetention <= schedule.DefaultCatchupMax {
		t.Errorf("coord.FireRetention %v can expire a claim a catchup pass could still "+
			"evaluate (catchup ceiling %v), so a scheduled fire runs twice",
			coord.FireRetention, schedule.DefaultCatchupMax)
	}

	// A rebase is inherited while it lies within the state log's mint
	// horizon of the attempt reading it, and the bucket's age counts from
	// the write, which is no earlier than the instant recorded. A bucket
	// that ages a record out inside that horizon sends the attempt after it
	// to mint anew — and to write a second copy of every write the attempt
	// that recorded it made.
	if coord.RebaseRetention <= statelog.MintHorizon {
		t.Errorf("coord.RebaseRetention %v can age out a rebase an attempt must still "+
			"inherit (mint horizon %v), so a retry writes its first attempt's writes twice",
			coord.RebaseRetention, statelog.MintHorizon)
	}
	// And it is sized FROM the ledger's retention rather than beside it: a
	// longer ledger lengthens the horizon, and the two must move together.
	if coord.RebaseRetention != statelog.OpsRetention {
		t.Errorf("coord.RebaseRetention %v has drifted from statelog.OpsRetention %v",
			coord.RebaseRetention, statelog.OpsRetention)
	}

	// A custody record names the data node that keeps a stateless node's
	// batch, and a node that never learned whether it kept one asks it
	// later. Aged out while the batch's rows are still in the event log, it
	// answers that nobody keeps the batch, and the asking node keeps a
	// second copy of what another node holds.
	if coord.CustodyRetention <= store.EventRetention {
		t.Errorf("coord.CustodyRetention %v can age out the keeper of a batch whose "+
			"rows the event log still holds (%v), so a node keeps a second copy",
			coord.CustodyRetention, store.EventRetention)
	}

	// The thread-follow horizon is the one here that is not sized from
	// another subsystem's cadence — it is sized from a fact about chat
	// products, that a quarter-old thread is reachable only through search
	// on every backend that ships one. What can still be checked is the
	// direction it must never drift in: a follow that expires inside a
	// conversation somebody is still having is a seat that goes quiet
	// mid-thread, which is the failure a person notices and cannot
	// diagnose. A month is far inside any live thread.
	if coord.FollowRetention <= 30*24*time.Hour {
		t.Errorf("coord.FollowRetention %v is short enough to expire a follow "+
			"inside a conversation that is still live, so a seat goes quiet "+
			"mid-thread and only a fresh mention brings it back",
			coord.FollowRetention)
	}
}
