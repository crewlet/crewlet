package coordtest_test

import (
	"testing"
	"time"

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

	// The setup-state window is bounded from BOTH sides, and neither bound
	// is a number this engine chooses. Above it is GitHub's: the one-time
	// code a manifest conversion returns dies at an hour, so a state valid
	// for longer lets somebody complete a flow whose code is already gone
	// — and the failure they are shown names the code rather than the
	// wait, which sends them to the wrong place entirely. Below it is a
	// person at a browser: a window that closes during a round trip to
	// GitHub and two clicks refuses the operator who did exactly what was
	// asked.
	//
	// It is checked here rather than in internal/api/setupapi because the
	// record is what enforces it: the claim that spends a state expires
	// with its bucket, so a manifestTTL longer than this retention would
	// be a token still validating after the record that spends it is gone
	// — which is the replay the record exists to stop. setupapi's
	// manifestTTL IS this constant for that reason.
	if coord.SetupOnceRetention >= time.Hour {
		t.Errorf("coord.SetupOnceRetention %v outlives GitHub's one-hour one-time code, so "+
			"a setup link can be completed after the code behind it is dead and the "+
			"operator is shown a failure naming the code rather than the wait",
			coord.SetupOnceRetention)
	}
	if coord.SetupOnceRetention < 5*time.Minute {
		t.Errorf("coord.SetupOnceRetention %v can close during the task it covers — a "+
			"browser round trip to the vendor and two clicks — so the operator who "+
			"did exactly what was asked is refused", coord.SetupOnceRetention)
	}

	// The authentication window and its cap are one decision in two
	// numbers, and the direction each must not drift in is what can be
	// checked. A window shorter than the interval a guessing run can wait
	// out between attempts is not a throttle; a cap at or below a
	// threshold anything would refuse at saturates before the throttle can
	// see the difference between "at the limit" and "far past it".
	if coord.AttemptWindow < time.Minute {
		t.Errorf("coord.AttemptWindow %v is short enough for a guessing run to wait out "+
			"between attempts and still make progress, which is a throttle that only "+
			"slows somebody down to its own window", coord.AttemptWindow)
	}
	if coord.AttemptCap < 5 {
		t.Errorf("coord.AttemptCap %d is at or below a lockout threshold anything would "+
			"refuse at, so the count a throttle reads saturates before it can tell "+
			"a caller at the limit from one far past it", coord.AttemptCap)
	}
	// 64 is the broker's own ceiling on a per-record history, and the cap
	// IS that history on the KV backend: a value above it is a bucket
	// nobody can create, which fails at boot rather than here.
	if coord.AttemptCap > 64 {
		t.Errorf("coord.AttemptCap %d exceeds the 64-message ceiling a KV bucket's "+
			"per-record history has, so the attempts bucket cannot be created at all",
			coord.AttemptCap)
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
