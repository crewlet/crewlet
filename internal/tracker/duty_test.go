package tracker_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY DUTY JOB IS GATED ON A FACT SOMEBODY WROTE DOWN.
//
// # Why that is the property rather than "the duty is correct"
//
// A duty that goes looking costs the same whether or not anything is wrong, on
// every node, for ever — and this one runs every fifteen minutes against a
// company with hundreds of thousands of tasks. So each job asks one indexed
// question first, and on a healthy company a whole tick is those questions and
// nothing else.
//
// The exception is named rather than hidden: the missed-unblocked repair has
// no flag to read, because the fact it looks for is a comparison between two
// instants rather than something a writer could have stamped. It is bounded by
// its own last position instead.
func TestEveryTrackerJobIsGatedOrSaysWhyNot(t *testing.T) {
	t.Parallel()
	jobs := tracker.Jobs(tracker.DutyDeps{NodeID: "node-a"})
	if len(jobs) == 0 {
		t.Fatal("the tracker registers no duty jobs, so this case is " +
			"measuring its own reader")
	}
	ungated := map[string]bool{
		// The repair has no flag to read: what it looks for is a
		// comparison between a blocker's clearing instant and what its
		// dependent was told, and neither is a column a writer stamps.
		"tracker_unblocked": true,
	}
	for _, job := range jobs {
		if job.Gate == nil && !ungated[job.Name] {
			t.Errorf("%s runs its own selection on every tick — a duty that "+
				"goes looking costs the same whether or not anything is "+
				"wrong, on every node, for ever", job.Name)
		}
		if job.PerNode {
			t.Errorf("%s is marked per-node and every tracker job publishes "+
				"RECORDS — running one on every node is N copies of one "+
				"record for the applier to arbitrate", job.Name)
		}
	}
}

// A TICK ON A HEALTHY COMPANY READS ITS GATES AND DOES NOTHING ELSE.
func TestAQuietTickRunsNoJob(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(swept) != 0 {
		t.Fatalf("a tick against a healthy company did %v — every job is "+
			"gated on a fact somebody wrote down, and nobody wrote one",
			swept)
	}
}

// THE DUTY FINISHES A RE-SPREAD THE INLINE WINDOW COULD NOT.
//
// A drag past the inline cap still succeeds with its long key, and the applier
// flags the project from that key's own length — on every node, so the fact is
// the fleet's rather than the writer's. The duty is what turns that flag into
// a walk.
func TestTheDutyWalksAFlaggedProject(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const total = 6
	for i := range total {
		if _, err := r.writer.CreateTask(t.Context(), fmt.Sprintf("op-%d", i),
			newTask(fmt.Sprintf("t-%02d", i)), nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}
	crowd(t, r, total)
	if !flagged(t, r, "rank_respread_pending") {
		t.Fatal("the applier did not flag a project whose keys are past the " +
			"threshold, so the duty has nothing to be triggered by")
	}

	worker := trackerWorker(t, r)
	swept, err := worker.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if swept["tracker_respread"] == 0 {
		t.Fatalf("the duty walked nothing against a flagged project: %v", swept)
	}
	r.drain()

	for _, rank := range boardRanks(t, r) {
		if len(rank) > tracker.RankRenormaliseAt {
			t.Errorf("the duty left rank %q at %d characters", rank, len(rank))
		}
	}
	if flagged(t, r, "rank_respread_pending") {
		t.Error("the project is still flagged after its walk, so the duty " +
			"walks it again on every tick for ever")
	}
}

// THE DUTY GIVES ONE OF TWO TASKS SHARING A RANK A FRESH KEY.
//
// A duplicate is a COSMETIC anomaly — two cards whose order is undefined
// between them — and it is repaired rather than refused, because a unique
// index here would turn it into a deterministic fleet-wide stalled log.
func TestTheDutyClearsADuplicateRank(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id,
			newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	// ONE RECORD PUTTING BOTH AT ONE KEY, which is what two concurrent
	// drags into the same gap produce on two nodes.
	if _, err := r.writer.MoveTasks(t.Context(), "op-collide", "ENG",
		[]tracker.Placement{
			{Task: "t-1", Rank: "a0V"}, {Task: "t-2", Rank: "a0V"},
		}); err != nil {
		t.Fatalf("MoveTasks: %v", err)
	}
	r.drain()
	if !flagged(t, r, "rank_duplicate_pending") {
		t.Fatal("the applier's own probe did not notice two tasks at one " +
			"rank, so nothing hands the repair to the duty")
	}

	if _, err := trackerWorker(t, r).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	r.drain()

	ranks := boardRanks(t, r)
	if len(ranks) != 2 || ranks[0] == ranks[1] {
		t.Fatalf("the board still reads %v — the repair mints a fresh key for "+
			"one of them so the order between the two is defined", ranks)
	}
}

// THE DUTY COMPLETES A MERGE WHOSE HOLDER DIED.
func TestTheDutyClearsAnAbandonedMerge(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id,
			newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	// THE MARKER WITHOUT THE REST, which is what a holder that died
	// between the first append and the last leaves behind.
	merging := true
	if _, err := r.writer.UpdateTask(t.Context(), "op-mark", "t-1", "ENG",
		tracker.TaskPatch{Merging: &merging}, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()

	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if swept["tracker_abandoned_merges"] == 0 {
		t.Fatalf("the duty completed no abandoned merge: %v — a task left "+
			"mid-merge is visibly half-merged for ever, which is the state "+
			"the marker exists to make somebody fix", swept)
	}
	r.drain()

	var merged int
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT merging FROM tracker_tasks WHERE id = 't-1'`).Scan(&merged)
	}); err != nil {
		t.Fatalf("read the marker: %v", err)
	}
	if merged != 0 {
		t.Error("the marker is still set after the duty completed the merge, " +
			"so the duty runs against it on every tick for ever")
	}
}

func trackerWorker(t *testing.T, r *roundTrip) *maintenance.Worker {
	t.Helper()
	return maintenance.New(maintenance.Options{
		Jobs: tracker.Jobs(tracker.DutyDeps{
			DB: r.db, Writer: r.writer, NodeID: "node-a",
		}),
		Now: func() time.Time { return wednesday },
	})
}

func flagged(t *testing.T, r *roundTrip, column string) bool {
	t.Helper()
	var set int
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT `+column+` FROM tracker_projects WHERE key = 'ENG'`).Scan(&set)
	}); err != nil {
		t.Fatalf("read %s: %v", column, err)
	}
	return set == 1
}
