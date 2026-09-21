package maintenance_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
)

// TestEveryDomainGetsBothNodeLocalSweeps.
//
// Both tables are NODE-LOCAL: each node holds its own operation rows and its
// own anchors, so a fleet singleton would tidy one node's copies and leave
// every other growing — which looks exactly like a sweep that works, to the
// operator who checks the node it ran on.
//
// The two differ in what bounds them, and that is what the Horizon assertion
// below pins: the ledger's cutoff is a CLOCK (how long a retrying client may
// take to re-ask), while the anchors' is the domain's own published trim
// floor, which the ledger reads for itself. A horizon on the anchor job would
// be a second opinion about what the log still holds.
func TestEveryDomainGetsBothNodeLocalSweeps(t *testing.T) {
	t.Parallel()
	ledger := &countingLedger{}
	jobs := maintenance.StatelogJobs(map[string]maintenance.OpsHorizon{
		"tracker": {Ledger: ledger, Retention: 30 * 24 * time.Hour},
		"pages":   {Ledger: ledger, Retention: 30 * 24 * time.Hour},
	})
	if len(jobs) != 4 {
		t.Fatalf("two domains produced %d job(s), want one ops sweep and one anchor "+
			"sweep each", len(jobs))
	}
	byName := map[string]maintenance.Job{}
	for _, j := range jobs {
		byName[j.Name] = j
		if j.Scope != maintenance.NodeLocal {
			t.Errorf("%s runs at %v scope — swept under the fleet singleton it would "+
				"be tidied on one node and grow for ever on every other",
				j.Name, j.Scope)
		}
	}
	for _, name := range []string{"tracker_ops", "pages_ops", "tracker_anchors", "pages_anchors"} {
		if _, held := byName[name]; !held {
			t.Errorf("no %s job", name)
		}
	}
	if byName["tracker_ops"].Horizon == 0 {
		t.Error("the ops sweep carries no horizon, so the worker has no cutoff to derive")
	}
	if byName["tracker_anchors"].Horizon != 0 {
		t.Error("the anchor sweep carries a horizon, which would be a second opinion " +
			"about what the log still holds — its cutoff is the published trim floor")
	}

	// AND BOTH ACTUALLY RUN, so a job that was registered and wired to
	// nothing is a failure here rather than a table that quietly grows.
	for _, name := range []string{"tracker_ops", "tracker_anchors"} {
		if _, err := byName[name].Run(t.Context(), time.Now(), time.Now()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if ledger.ops != 1 || ledger.anchors != 1 {
		t.Errorf("the jobs called PurgeOps %d time(s) and PurgeAnchors %d, want one each",
			ledger.ops, ledger.anchors)
	}
}

type countingLedger struct{ ops, anchors int }

func (l *countingLedger) PurgeOps(context.Context, time.Time) (int64, error) {
	l.ops++
	return 0, nil
}

func (l *countingLedger) PurgeAnchors(context.Context) (int64, error) {
	l.anchors++
	return 0, nil
}
