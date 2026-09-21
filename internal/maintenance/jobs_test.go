package maintenance_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/store"
)

// openStore is a real node estate, because the two jobs below are wired to
// store methods and a stub would certify the wiring against itself.
func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "m.db"), store.Options{})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

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

// A REVISION OUTLIVES THE AUDIT ROW THAT NAMES IT.
//
// # The relation, which is the thing to keep — not the number
//
// The event store records which revision was activated, by whom and when, and
// an operator reading that row a month later follows it to the document. If
// the revision sweep ran shorter than the audit log's own horizon, that
// follow would land on a row the sweep had already deleted — an audit trail
// pointing at nothing, which is worse than one that stops, because it reads
// as damage.
//
// THE TWO CONSTANTS ARE IN DIFFERENT PACKAGES and nothing else connects them.
// [maintenance.RevisionRetention] is set here from an annual cycle and
// [store.EventRetention] is set there from the event log's read floor, so
// either can move for a reason that has nothing to do with the other. This is
// what makes the ordering between them a decision rather than a coincidence
// that happens to hold today.
//
// The margin is deliberately not asserted: what must hold is the ORDER. A
// test demanding a particular multiple would fail for a change that is
// perfectly sound and would have to be edited to say so, which is how a
// relation quietly becomes a number nobody believes.
func TestARevisionOutlivesTheAuditRowThatNamesIt(t *testing.T) {
	t.Parallel()

	if maintenance.RevisionRetention <= store.EventRetention {
		t.Errorf("revisions are kept for %v and the audit log for %v.\n"+
			"An audit row naming a revision would outlive the revision, so "+
			"following one lands on a row that was swept — which reads as a "+
			"corrupted store rather than as a retention policy. Raise "+
			"RevisionRetention, or lower the event log's horizon.",
			maintenance.RevisionRetention, store.EventRetention)
	}
}

// AND THE SWEEP OF THE REVISION TABLE IS NODE-LOCAL.
//
// Each node adopts its OWN copy of every revision it meets, so a fleet
// singleton would tidy the node holding the duty and let the table grow for
// ever on every other — which looks exactly like a sweep that works, to the
// operator who checks the node it ran on. It is the same mistake six sweeps
// made by omission before the scope became a required answer.
func TestTheRevisionSweepRunsOnEveryNode(t *testing.T) {
	t.Parallel()

	// A NIL DATABASE CONTRIBUTES NOTHING rather than a job that fails
	// every tick, which is what every other job constructor here does: a
	// deployment with no store is a real deployment.
	if jobs := maintenance.ConfigJobs(nil); jobs != nil {
		t.Errorf("a nil store produced %d job(s)", len(jobs))
	}

	db := openStore(t)
	jobs := maintenance.ConfigJobs(db)
	if len(jobs) != 1 {
		t.Fatalf("ConfigJobs produced %d jobs, want the one over company_config", len(jobs))
	}
	job := jobs[0]
	if job.Name != "company_config" {
		t.Errorf("job name = %q, want the table it sweeps", job.Name)
	}
	if job.Scope != maintenance.NodeLocal {
		t.Errorf("scope = %q, want %q — every node holds its own copy of "+
			"every revision it has ever met", job.Scope, maintenance.NodeLocal)
	}
	if job.Horizon != maintenance.RevisionRetention {
		t.Errorf("horizon = %v, want %v", job.Horizon, maintenance.RevisionRetention)
	}
	// AND IT ACTUALLY REACHES THE TABLE, which a job wired to the wrong
	// store method would not: a sweep that never errors and never deletes
	// is indistinguishable from one that works.
	if _, err := job.Run(t.Context(), time.Now(), time.Now()); err != nil {
		t.Errorf("the job failed against a real store: %v", err)
	}
}
