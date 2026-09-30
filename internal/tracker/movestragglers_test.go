package tracker_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// strandBehindAMove files a subtask under m-kid-a on a writer whose snapshot
// predates m-root's move into OPS, and lets the broker accept it only once the
// whole move — its last append, which takes the mark down, included — has
// landed and applied. That is the one ordering the move's own walk cannot see:
// the create's decide read its parent unmarked in ENG, and its record arrived
// after the walk had finished checking.
func strandBehindAMove(t *testing.T, r *roundTrip, id string) {
	t.Helper()
	lossy, log := r.lossyWriter(t)
	log.beforeAppendTo(id, func() {
		if _, err := r.writer.MoveTaskToProject(t.Context(),
			statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil); err != nil {
			t.Errorf("the move the task is filed behind: %v", err)
		}
		r.drain()
	})
	under := "m-kid-a"
	late := newTask(id)
	late.Key = ""
	late.Parent, late.Depth = &under, 2
	late.Tags = []string{"late-tag"}
	if _, err := lossy.CreateTask(t.Context(), "op-"+id, late, nil); err != nil {
		t.Fatalf("file %s behind the move: %v", id, err)
	}
	r.drain()

	root, straggler := oneTask(t, r, "m-root"), oneTask(t, r, id)
	switch {
	case root.Project != "OPS" || root.Moving:
		t.Fatalf("the premise: the root is in %q marked %v, want OPS with its "+
			"move finished", root.Project, root.Moving)
	case straggler.Project != "ENG":
		t.Fatalf("the premise: %s is in %q, want ENG — filed behind the move",
			id, straggler.Project)
	}
	if got := r.strings(`SELECT id FROM tracker_tasks WHERE inconsistent_project = 1`); len(got) != 1 || got[0] != id {
		t.Fatalf("the premise: the flagged tasks are %v, want %s", got, id)
	}
}

// A TASK FILED BEHIND A MOVE THAT HAS FINISHED IS CARRIED BY THE DUTY.
//
// A create decided on a node that had not applied the root's move, and
// accepted by the broker after the move's last append took the mark down,
// lands in the old project under a root in the new one. The applier flags it
// `inconsistent_project` and applies it — a committed record is never refused
// there — and with the mark down the abandoned-move job never looked at that
// root again: the task stayed in ENG for good, on neither project's board.
//
// Mutation: drop tracker_move_stragglers from Jobs and the tick carries
// nothing (and the gate helper finds no such job).
func TestATaskFiledBehindAFinishedMoveIsCarried(t *testing.T) {
	t.Parallel()
	r := lateTagFixture(t)
	strandBehindAMove(t, r, "m-late")
	if trackerGate(t, r, "tracker_abandoned_moves") {
		t.Fatal("the premise: the abandoned-move job sees a move to finish, so " +
			"this is not the ordering its mark cannot cover")
	}
	if !trackerGate(t, r, "tracker_move_stragglers") {
		t.Fatal("the straggler gate reports nothing to do with m-late in ENG " +
			"under a root in OPS")
	}

	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := swept["tracker_move_stragglers"]; got != 1 {
		t.Fatalf("the duty carried stragglers under %d root(s), want 1: %v", got, swept)
	}
	r.drain()
	late := oneTask(t, r, "m-late")
	if late.Project != "OPS" || !strings.HasPrefix(late.Key, "OPS-") {
		t.Errorf("m-late is %s in %q after the duty, want an OPS key in OPS",
			late.Key, late.Project)
	}
	if got := r.strings(`SELECT id FROM tracker_tasks WHERE inconsistent_project = 1`); len(got) != 0 {
		t.Errorf("tasks %v are still in another project than their root", got)
	}
	// ITS TAGS WENT WITH IT, as a move's own pass declares them.
	if got := r.strings(`SELECT label FROM tracker_tags
		WHERE project_key = 'OPS' AND slug = 'late-tag'`); len(got) != 1 || got[0] != "Late" {
		t.Errorf("OPS declares late-tag as %v, want it as ENG does", got)
	}
	if oneTask(t, r, "m-root").Moving {
		t.Error("the repair marked the root mid-move")
	}
	if trackerGate(t, r, "tracker_move_stragglers") {
		t.Error("the gate still reports a straggler, so this job runs on every " +
			"tick for ever")
	}
}

// A STRAGGLER IN THE TRASH IS LEFT UNTIL ITS RESTORE, and costs no tick.
//
// It takes no write, so the repair could not carry it; and with the move's
// mark down there is no walk to hold open for it — so it is neither waited
// for nor reported on every tick, and its restore is what brings it back.
func TestAStragglerInTheTrashIsCarriedAfterItsRestore(t *testing.T) {
	t.Parallel()
	r := lateTagFixture(t)
	strandBehindAMove(t, r, "m-late")
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove-late", "m-late", "ENG",
		false, nil); err != nil {
		t.Fatalf("put the straggler in the trash: %v", err)
	}
	r.drain()
	if trackerGate(t, r, "tracker_move_stragglers") {
		t.Error("the gate reports a straggler that is in the trash — the job " +
			"would run on every tick until somebody restores it")
	}

	if _, err := r.writer.RestoreTask(t.Context(), "op-restore-late", "m-late", "ENG",
		nil); err != nil {
		t.Fatalf("restore the straggler: %v", err)
	}
	r.drain()
	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := swept["tracker_move_stragglers"]; got != 1 {
		t.Errorf("the duty carried stragglers under %d root(s) after the "+
			"restore, want 1", got)
	}
	r.drain()
	if late := oneTask(t, r, "m-late"); late.Project != "OPS" {
		t.Errorf("m-late is in %q after its restore and the duty, want OPS",
			late.Project)
	}
}

// THE REPAIR RUNS ONLY UNDER THE MOVE'S OWN CLAIM, so a move of that root that
// is running now is left alone this tick, exactly as the abandoned-move job
// leaves one — the two would otherwise walk one subtree at once.
func TestTheStragglerRepairLeavesARootWhoseMoveHoldsItsClaim(t *testing.T) {
	t.Parallel()
	r := lateTagFixture(t)
	strandBehindAMove(t, r, "m-late")
	lease, err := r.claims.TryAcquire(t.Context(), tracker.MoveClaim("m-root"),
		coord.AcquireOptions{Owner: "node-b", TTL: tracker.ClaimTTL})
	if err != nil || lease == nil {
		t.Fatalf("hold the move's claim: (%v, %v)", lease, err)
	}
	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	r.drain()
	if got := swept["tracker_move_stragglers"]; got != 0 ||
		oneTask(t, r, "m-late").Project != "ENG" {
		t.Errorf("the duty carried %d root(s)' stragglers under a claim somebody "+
			"else holds", got)
	}

	if _, err := r.claims.Release(t.Context(), tracker.MoveClaim("m-root"),
		"node-b", lease.Epoch); err != nil {
		t.Fatalf("release the claim: %v", err)
	}
	if swept, err = trackerWorker(t, r).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	r.drain()
	if got := swept["tracker_move_stragglers"]; got != 1 ||
		oneTask(t, r, "m-late").Project != "OPS" {
		t.Errorf("once the claim lapsed the duty carried %d root(s)' stragglers "+
			"and m-late is in %q, want 1 and OPS", got,
			oneTask(t, r, "m-late").Project)
	}
}
