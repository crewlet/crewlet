package tracker_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/tracker"
)

// TestTheInboxSweepDeletesWhatAgedOutAndNothingElse is the finding. The table
// shipped `tracker_notifications_swept_idx ON (created_at)` for "the per-node
// inbox retention sweep, which is a range delete", the horizon is a validated
// company setting with a default and bounds, and its own doc calls it "the one
// horizon here that deletes anything" — and nothing deleted anything. Every
// routed change wrote a row per recipient and kept it for the life of the
// deployment, on every node.
func TestTheInboxSweepDeletesWhatAgedOutAndNothingElse(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// TWO NOTICES A YEAR APART, authored by moving the harness's clock —
	// the sweep compares on the AUTHORED instant, which is the column the
	// index is on and the one the applier's own horizon compares against.
	old := wednesday.Add(-400 * 24 * time.Hour)
	r.at = old
	routeTo(t, r, "t-1", "ENG-1", "bob")
	r.at = wednesday
	routeTo(t, r, "t-2", "ENG-2", "bob")

	before := r.inbox(tracker.InboxQuery{Handle: "bob"})
	if len(before.Notices) != 2 {
		t.Fatalf("bob's inbox holds %d notices, want 2", len(before.Notices))
	}

	// THE HORIZON IS ASKED AT EVERY TICK, because the applier reads it off
	// the epoch at every batch and the two must agree: a first tick under
	// a longer horizon keeps the year-old notice, and the next, after the
	// company shortened it, deletes it — the same job, rebuilt by nothing.
	horizon := 500 * 24 * time.Hour
	jobs := tracker.InboxJobs(r.db, func() time.Duration { return horizon })
	if len(jobs) != 1 {
		t.Fatalf("InboxJobs returned %d jobs, want 1", len(jobs))
	}
	// NODE-LOCAL, because `tracker_notifications` is Divergent: each node
	// holds its own rows, so a fleet singleton would sweep one and let the
	// table grow for ever on every other — which looks exactly like a
	// sweep that works to the operator who checks the node it ran on.
	if jobs[0].Scope != maintenance.NodeLocal {
		t.Fatalf("the inbox sweep has scope %q, so it tidies one node's "+
			"rows and lets every peer's grow for ever", jobs[0].Scope)
	}
	// NO HORIZON OF ITS OWN, so the worker hands it no cutoff to trust:
	// a static one is the value that stayed at the boot company's.
	if jobs[0].Horizon != 0 {
		t.Fatalf("the job declares a horizon of %s, which the worker would "+
			"hold for the life of the process", jobs[0].Horizon)
	}

	// THE SWEEP RUNS BESIDE A LIVE APPLIER, which holds the estate's pin
	// for the life of the process: a sweep that asked for one of its own
	// was refused on every tick of a running node.
	holdTheAppliersPin(t, r)
	// The cutoff argument is the worker's, derived from a horizon the job
	// does not declare, so it is deliberately nonsense here: the job must
	// derive its own from the instant.
	swept, err := jobs[0].Run(t.Context(), wednesday, wednesday)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 0 {
		t.Fatalf("the sweep deleted %d rows under a 500-day horizon, want "+
			"none — the oldest notice is 400 days old", swept)
	}
	horizon = 365 * 24 * time.Hour
	swept, err = jobs[0].Run(t.Context(), wednesday, wednesday)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 1 {
		t.Fatalf("the sweep deleted %d rows, want the one past the horizon",
			swept)
	}

	after := r.inbox(tracker.InboxQuery{Handle: "bob"})
	if len(after.Notices) != 1 {
		t.Fatalf("bob's inbox holds %d notices after the sweep, want 1",
			len(after.Notices))
	}
	if after.Notices[0].SubjectKey != "ENG-2" {
		t.Fatalf("the sweep kept %s, want the recent ENG-2",
			after.Notices[0].SubjectKey)
	}

	// AND THE HISTORY IS UNTOUCHED, which is what the config doc
	// promises: a notice is a POINTER at a history row, and the history
	// answers for ever.
	if got := r.strings(`SELECT id FROM tracker_history
		WHERE subject_id = 'ENG-1' OR subject_id = 't-1'`); len(got) == 0 {

		t.Fatal("the sweep deleted the history the notice pointed at")
	}

	// A SECOND SWEEP IS A NO-OP, so a tick with nothing to do costs one
	// range delete and reports zero rather than re-reporting the first.
	again, err := jobs[0].Run(t.Context(), wednesday, wednesday)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again != 0 {
		t.Fatalf("a second sweep deleted %d rows", again)
	}
}

// TestTheInboxSweepDeclinesWithNoStore keeps the nil guard honest: a node with
// no store contributes no job rather than a job that panics on its first tick.
func TestTheInboxSweepDeclinesWithNoStore(t *testing.T) {
	t.Parallel()
	if jobs := tracker.InboxJobs(nil, func() time.Duration { return time.Hour }); jobs != nil {
		t.Fatalf("a node with no store contributed %d sweep jobs", len(jobs))
	}
}

// A HORIZON NOBODY STATED SWEEPS NOTHING. Zero is the answer of a source with
// no company behind it, and read as a duration it is "retain nothing" — the
// cutoff would be the tick's own instant and every notice would go.
func TestAnInboxHorizonOfZeroSweepsNothing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.at = wednesday
	routeTo(t, r, "t-1", "ENG-1", "bob")
	holdTheAppliersPin(t, r)
	jobs := tracker.InboxJobs(r.db, func() time.Duration { return 0 })
	swept, err := jobs[0].Run(t.Context(), wednesday.Add(time.Hour), wednesday)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 0 {
		t.Fatalf("a zero horizon deleted %d notices", swept)
	}
}
