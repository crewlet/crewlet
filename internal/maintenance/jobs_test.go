package maintenance_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// openStore is a real node estate, because the two jobs below are wired to
// store methods and a stub would certify the wiring against itself.
func openStore(t *testing.T) *store.DB {
	t.Helper()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "m.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestEveryDomainGetsANodeLocalLedgerSweepAndNoAnchorSweep.
//
// The operation ledger is NODE-LOCAL: a fleet singleton would tidy one node's
// copy and leave every other growing, which looks like a sweep that works. Its
// cutoff is a CLOCK (how long a retrying client may take to re-ask), so the job
// carries the domain's horizon.
//
// AND NO ANCHOR SWEEP. Arbitration anchors are replicated rows, which only an
// applier writes (ADR-0002): the trim removes them with a trimmed record every
// node applies at one position (ADR-0080); a job deleting them below a floor
// each node read for itself would leave two copies of one log with different
// anchors.
func TestEveryDomainGetsANodeLocalLedgerSweepAndNoAnchorSweep(t *testing.T) {
	t.Parallel()
	ledger := &countingLedger{}
	jobs := maintenance.StatelogJobs(map[string]maintenance.OpsHorizon{
		"tracker": {Ledger: ledger, Retention: 30 * 24 * time.Hour},
		"pages":   {Ledger: ledger, Retention: 30 * 24 * time.Hour},
	})
	if len(jobs) != 2 {
		t.Fatalf("two domains produced %d job(s), want one ops sweep each", len(jobs))
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
	for _, name := range []string{"tracker_ops", "pages_ops"} {
		if _, held := byName[name]; !held {
			t.Errorf("no %s job", name)
		}
	}
	for _, name := range []string{"tracker_anchors", "pages_anchors"} {
		if _, held := byName[name]; held {
			t.Errorf("a %s job sweeps replicated rows on this node's own clock, a second "+
				"writer beside the trimmed record every applier takes", name)
		}
	}
	if h := byName["tracker_ops"].Horizon; h == nil || h() != 30*24*time.Hour {
		t.Error("the ops sweep carries no horizon of the domain's, so the worker has no " +
			"cutoff to derive")
	}

	// AND IT ACTUALLY RUNS, so a job that was registered and wired to
	// nothing is a failure here rather than a table that quietly grows.
	if _, err := byName["tracker_ops"].Run(t.Context(), time.Now(), time.Now()); err != nil {
		t.Errorf("tracker_ops: %v", err)
	}
	if ledger.ops != 1 {
		t.Errorf("the job called PurgeOps %d time(s), want one", ledger.ops)
	}
}

// horizonOf is what a job's horizon answers now, and zero for a job that
// carries none.
func horizonOf(j maintenance.Job) time.Duration {
	if j.Horizon == nil {
		return 0
	}
	return j.Horizon()
}

type countingLedger struct {
	ops int
	// kinds is which kinds PurgeOpsOfKind was asked about.
	kinds []string
}

func (l *countingLedger) PurgeOps(context.Context, time.Time) (int64, error) {
	l.ops++
	return 0, nil
}

func (l *countingLedger) PurgeOpsOfKind(_ context.Context, kind string,
	_ time.Time) (int64, error) {

	l.kinds = append(l.kinds, kind)
	return 0, nil
}

// A SUBJECT KIND WITH A HORIZON OF ITS OWN IS A SWEEP OF ITS OWN.
//
// The ledger's horizon is sized for the slowest client that re-asks an op id —
// a seat, after a weekend — and a kind no such client writes (the identity
// domain's sessions: one row per sign-in, re-asked inside the publisher's
// resolve budget if at all) would otherwise keep a month of rows for a question
// nobody asks after an hour. So each declared kind is its own node-local job,
// at ITS horizon, asking the ledger about THAT kind — and the domain's default
// job is still there for everything else.
func TestASubjectKindWithItsOwnHorizonIsItsOwnSweep(t *testing.T) {
	t.Parallel()
	ledger := &countingLedger{}
	jobs := maintenance.StatelogJobs(map[string]maintenance.OpsHorizon{
		"iam": {Ledger: ledger, Retention: 30 * 24 * time.Hour,
			Kinds: map[string]time.Duration{"session": time.Hour}},
	})
	byName := map[string]maintenance.Job{}
	for _, j := range jobs {
		byName[j.Name] = j
	}
	kind, held := byName["iam_ops_session"]
	if !held {
		t.Fatalf("no iam_ops_session job among %v", jobs)
	}
	if kind.Horizon == nil || kind.Horizon() != time.Hour || kind.Scope != maintenance.NodeLocal {
		t.Errorf("the session sweep runs at %v scope with horizon %v, want "+
			"node-local and the kind's own hour", kind.Scope, horizonOf(kind))
	}
	if h := horizonOf(byName["iam_ops"]); h != 30*24*time.Hour {
		t.Errorf("the default ops sweep's horizon is %s, want the domain's", h)
	}
	if _, err := kind.Run(t.Context(), time.Now(), time.Now()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(ledger.kinds) != 1 || ledger.kinds[0] != "session" || ledger.ops != 0 {
		t.Errorf("the session sweep asked the ledger about kinds %v and ran "+
			"the whole-ledger purge %d time(s), want the one kind and none",
			ledger.kinds, ledger.ops)
	}
}

// A REVISION OUTLIVES THE AUDIT ROW THAT NAMES IT.
//
// An audit row names the revision activated; a revision sweep shorter than the
// audit log's horizon would leave a trail pointing at nothing, which reads as
// damage. The relation is what to keep, not the number: [maintenance.RevisionRetention]
// (an annual cycle) and [store.EventRetention] (the event log's read floor) are
// in different packages and either can move for its own reason.
//
// The margin is deliberately not asserted, only the ORDER: a test demanding a
// multiple would fail for a sound change and turn a relation into a number
// nobody believes.
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
	if h := horizonOf(job); h != maintenance.RevisionRetention {
		t.Errorf("horizon = %v, want %v", h, maintenance.RevisionRetention)
	}
	// AND IT ACTUALLY REACHES THE TABLE, which a job wired to the wrong
	// store method would not: a sweep that never errors and never deletes
	// is indistinguishable from one that works.
	if _, err := job.Run(t.Context(), time.Now(), time.Now()); err != nil {
		t.Errorf("the job failed against a real store: %v", err)
	}
}
