package coordtest_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/schedule"
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
}
