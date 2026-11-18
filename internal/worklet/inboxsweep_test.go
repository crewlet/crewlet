package worklet_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
)

// TestTheInboxSweepDeletesWhatAgedOutAndNothingElse: rows past the company's
// inbox horizon are range-deleted on every node, and nothing else is.
func TestTheInboxSweepDeletesWhatAgedOutAndNothingElse(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// TWO NOTICES A YEAR APART, authored by moving the harness's clock: the
	// sweep compares the AUTHORED instant, as the applier's horizon does.
	old := wednesday.Add(-400 * 24 * time.Hour)
	r.at = old
	routeTo(t, r, "t-1", "ENG-1", "bob")
	r.at = wednesday
	routeTo(t, r, "t-2", "ENG-2", "bob")

	before := r.inbox(worklet.InboxQuery{Handle: "bob"})
	if len(before.Notices) != 2 {
		t.Fatalf("bob's inbox holds %d notices, want 2", len(before.Notices))
	}

	// THE HORIZON IS ASKED AT EVERY TICK, because the applier reads it off
	// the epoch at every batch and the two must agree: a first tick under a
	// longer horizon keeps the year-old notice, and the next, after the company
	// shortened it, deletes it, from the same job.
	horizon := 500 * 24 * time.Hour
	jobs := worklet.InboxJobs(r.db, func() time.Duration { return horizon })
	if len(jobs) != 1 {
		t.Fatalf("InboxJobs returned %d jobs, want 1", len(jobs))
	}
	// NODE-LOCAL, because `tracker_notifications` is Divergent: a fleet
	// singleton would sweep one node and let every other grow for ever.
	if jobs[0].Scope != maintenance.NodeLocal {
		t.Fatalf("the inbox sweep has scope %q, so it tidies one node's "+
			"rows and lets every peer's grow for ever", jobs[0].Scope)
	}
	// THE JOB'S HORIZON IS THE SOURCE, asked rather than copied, or it stays
	// at the boot company's.
	if jobs[0].Horizon == nil {
		t.Fatal("the job declares no horizon, so the worker would hand it " +
			"the sweep's own instant as its cutoff and every notice would go")
	}
	if got := jobs[0].Horizon(); got != horizon {
		t.Fatalf("the job's horizon is %s, want the retention it was given", got)
	}

	// THE SWEEP RUNS BESIDE A LIVE APPLIER, which holds the estate's pin for
	// the process's life, so a sweep asking for its own would be refused.
	holdTheAppliersPin(t, r)
	// The cutoff is derived as the worker derives it, from the horizon asked
	// at this sweep.
	sweep := func() (int64, error) {
		return jobs[0].Run(t.Context(), wednesday, wednesday.Add(-jobs[0].Horizon()))
	}
	swept, err := sweep()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 0 {
		t.Fatalf("the sweep deleted %d rows under a 500-day horizon, want "+
			"none — the oldest notice is 400 days old", swept)
	}
	horizon = 365 * 24 * time.Hour
	swept, err = sweep()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 1 {
		t.Fatalf("the sweep deleted %d rows, want the one past the horizon",
			swept)
	}

	after := r.inbox(worklet.InboxQuery{Handle: "bob"})
	if len(after.Notices) != 1 {
		t.Fatalf("bob's inbox holds %d notices after the sweep, want 1",
			len(after.Notices))
	}
	if after.Notices[0].SubjectKey != "ENG-2" {
		t.Fatalf("the sweep kept %s, want the recent ENG-2",
			after.Notices[0].SubjectKey)
	}

	// AND THE HISTORY IS UNTOUCHED, as the config doc promises: a notice is a
	// POINTER at a history row.
	if got := r.strings(`SELECT id FROM tracker_history
		WHERE subject_id = 'ENG-1' OR subject_id = 't-1'`); len(got) == 0 {

		t.Fatal("the sweep deleted the history the notice pointed at")
	}

	// A SECOND SWEEP IS A NO-OP, reporting zero rather than re-reporting the
	// first.
	again, err := sweep()
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again != 0 {
		t.Fatalf("a second sweep deleted %d rows", again)
	}
}

// TestTheInboxSweepDeclinesWithNoStore: a node with no tracker contributes no
// job rather than one that fails on its first tick.
func TestTheInboxSweepDeclinesWithNoStore(t *testing.T) {
	t.Parallel()
	if jobs := worklet.InboxJobs(store.ReplicatedHandle{}, maintenance.Fixed(time.Hour)); jobs != nil {
		t.Fatalf("a node with no tracker contributed %d sweep jobs", len(jobs))
	}
}
