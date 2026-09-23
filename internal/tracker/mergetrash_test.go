package tracker_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A MERGE INTO AN ITEM IN THE TRASH IS REFUSED BEFORE THE MARK, WHICHEVER WAY
// THE SUBTASKS GO.
//
// Moving them, every re-parent is refused on its own subject — a parent in
// the trash takes no new child — so the mark was one nothing could finish: the
// duplicate stayed `merging` and the duty ran the same refusal on every tick.
// Leaving them, the one live copy of the work was cancelled in favour of one
// nobody can see. Both used to be accepted.
func TestAMergeIntoAnItemInTheTrashIsRefusedBeforeTheMark(t *testing.T) {
	t.Parallel()
	for _, reparent := range []bool{true, false} {
		t.Run(map[bool]string{true: "moving the subtasks",
			false: "leaving the subtasks"}[reparent], func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			r.applyWhileWriting()
			filedTask(t, r, "keep")
			filedTask(t, r, "dup")
			under := "dup"
			kid := newTask("kid")
			kid.Parent = &under
			if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
				t.Fatalf("CreateTask kid: %v", err)
			}
			r.drain()
			if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "keep",
				"ENG", false, nil); err != nil {
				t.Fatalf("RemoveTask: %v", err)
			}
			r.drain()

			_, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup",
				"keep", reparent, nil)
			switch {
			case err == nil:
				t.Fatal("a duplicate was merged into an item in the trash")
			case errors.Is(err, statelog.ErrUnavailable):
				t.Errorf("the refusal %v says to come back, and waiting "+
					"restores nothing", err)
			case !strings.Contains(err.Error(), "restore"):
				t.Errorf("the refusal %q does not say to restore the survivor, "+
					"which is the one move that makes the merge possible", err)
			}
			r.drain()
			dup := oneTask(t, r, "dup")
			if dup.Merging {
				t.Error("the refused merge marked the duplicate anyway, and " +
					"nothing can finish that mark")
			}
			if dup.Status != tracker.StatusTodo {
				t.Errorf("the duplicate is %q after a refused merge", dup.Status)
			}
		})
	}
}

// A MERGE THAT MOVES THE SUBTASKS IS NOT MADE INTO ONE OF THEM.
//
// The subtask the survivor sits under would be re-parented onto the survivor
// itself — a cycle, which a re-parent now refuses — so the mark would stand
// with nothing to finish it. Left where they are, nothing moves and the fold
// is ordinary.
func TestAMergeCannotMoveTheSubtasksOntoOneOfThem(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "dup")
	under := "dup"
	kid := newTask("kid")
	kid.Parent = &under
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
	}
	r.drain()

	_, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "kid",
		true, nil)
	if err == nil || !strings.Contains(err.Error(), "subtasks") {
		t.Fatalf("a merge moving the subtasks onto one of them = %v, want a "+
			"refusal naming the subtasks", err)
	}
	r.drain()
	if oneTask(t, r, "dup").Merging {
		t.Error("the refused merge marked the duplicate anyway")
	}

	if _, err := r.writer.MergeDuplicates(t.Context(), "op-stay", "dup", "kid",
		false, nil); err != nil {
		t.Fatalf("a merge into its own subtask that leaves the subtasks = %v", err)
	}
	r.drain()
	if got := parentOf(r.task(t, "kid")); got != "dup" {
		t.Errorf("the subtask's parent is %q after a merge told to leave it", got)
	}
}

// A TASK IS NEVER FILED UNDER ITSELF OR ITS OWN SUBTREE.
//
// The applier cannot refuse a committed record, so it applies the cycle and
// flags it — and the subtree drops off every board, because a board draws
// roots and a cycle has none. Two concurrent re-parents on two subjects can
// still form one no single write sees; ONE write forming it was simply a write
// nobody refused.
func TestATaskIsNeverFiledUnderItsOwnSubtree(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "top")
	parent := "top"
	mid := newTask("mid")
	mid.Parent = &parent
	if _, err := r.writer.CreateTask(t.Context(), "op-mid", mid, nil); err != nil {
		t.Fatalf("CreateTask mid: %v", err)
	}
	r.drain()
	under := "mid"
	low := newTask("low")
	low.Parent = &under
	if _, err := r.writer.CreateTask(t.Context(), "op-low", low, nil); err != nil {
		t.Fatalf("CreateTask low: %v", err)
	}
	r.drain()

	for _, onto := range []string{"top", "mid", "low"} {
		onto := onto
		_, err := r.writer.UpdateTask(t.Context(), "op-onto-"+onto, "top", "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Parent: &onto},
			tracker.ChangeReparented, nil)
		switch {
		case err == nil:
			t.Errorf("top was filed under %s, which is in its own subtree", onto)
		case errors.Is(err, statelog.ErrUnavailable):
			t.Errorf("filing top under %s = %v, which says to come back to a "+
				"refusal waiting cannot change", onto, err)
		case !strings.Contains(err.Error(), "cycle"):
			t.Errorf("filing top under %s = %q, which does not say why", onto, err)
		}
	}
	r.drain()
	if got := oneTask(t, r, "top"); got.Parent != nil {
		t.Errorf("top took parent %q from inside its own subtree", *got.Parent)
	}
}

// A PURGED PARENT IS REFUSED, NOT WAITED FOR — and one this node has never
// heard of IS waited for.
//
// The two absences are different facts. A purge leaves a marker that outlives
// the row, so a task carrying one is gone on every node and for good; it was
// answered "not on this node", which told a caller to come back to a task that
// will never return. Only a task with no row and no marker may be one this
// node has not applied yet.
func TestAPurgedParentIsRefusedAndAnUnknownOneIsWaitedFor(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "gone")
	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", "gone", "ENG",
		"a test"); err != nil {
		t.Fatalf("PurgeTask: %v", err)
	}
	r.drain()

	parent := "gone"
	orphan := newTask("orphan")
	orphan.Parent = &parent
	_, err := r.writer.CreateTask(t.Context(), "op-orphan", orphan, nil)
	switch {
	case err == nil:
		t.Fatal("a subtask was filed under a purged task")
	case errors.Is(err, statelog.ErrUnavailable):
		t.Errorf("a purged parent answered %v — come back later, to a task a "+
			"purge destroyed for good", err)
	case !strings.Contains(err.Error(), "purged"):
		t.Errorf("the refusal %q does not say the parent was purged", err)
	}

	elsewhere := "never-applied-here"
	early := newTask("early")
	early.Parent = &elsewhere
	if _, err := r.writer.CreateTask(t.Context(), "op-early", early, nil); !errors.Is(err,
		statelog.ErrUnavailable) {
		t.Errorf("a parent this node holds no row or marker of = %v, want the "+
			"unavailable answer — it may be a create this node has not applied",
			err)
	}
}

// A CHILD IN THE TRASH STAYS WHERE IT IS, AND THE MERGE STILL FINISHES.
//
// A tombstoned task is frozen, so a re-parent of one is refused on its own
// subject — and the walk used to select it, fail on it, and leave the
// duplicate marked for a duty that would fail on it again every tick.
func TestAMergeLeavesASubtaskInTheTrashWhereItIs(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	under := "dup"
	for _, id := range []string{"kid-live", "kid-trashed"} {
		kid := newTask(id)
		kid.Parent = &under
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, kid, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "kid-trashed",
		"ENG", false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		true, nil); err != nil {
		t.Fatalf("a merge whose duplicate has a subtask in the trash = %v", err)
	}
	r.drain()
	if got := parentOf(r.task(t, "kid-live")); got != "keep" {
		t.Errorf("the live subtask's parent is %q, want keep", got)
	}
	if got := parentOf(r.task(t, "kid-trashed")); got != "dup" {
		t.Errorf("the subtask in the trash moved to %q; a tombstoned task is "+
			"frozen, and a restore brings it back under the duplicate", got)
	}
	dup := oneTask(t, r, "dup")
	if dup.Merging || dup.Status != tracker.StatusCancelled {
		t.Errorf("the duplicate is %q and merging=%v; the merge should have "+
			"finished", dup.Status, dup.Merging)
	}
}

// A MERGE WAITING ON THE TRASH IS REPORTED AND STEPPED OVER, AND THE ONE
// BEHIND IT FINISHES.
//
// The survivor went to the trash after the mark, which no pre-flight can
// prevent. The duty ran that merge's refusal every tick and RETURNED on it, in
// id order — so every abandoned merge sorting after it waited on a restore
// nobody knew to make. Now it is named on every tick and does not stand in the
// way; restoring its survivor is what lets it finish.
func TestTheDutyReportsAMergeWaitingOnTheTrashAndFinishesTheRest(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"a-keep", "a-dup", "b-keep", "b-dup"} {
		filedTask(t, r, id)
	}
	for _, pair := range [][2]string{{"a-dup", "a-keep"}, {"b-dup", "b-keep"}} {
		markMerging(t, r, pair[0], pair[1], true)
	}
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "a-keep", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	var logged bytes.Buffer
	worker := loggedTrackerWorker(t, r, &logged)
	swept, err := worker.Tick(t.Context())
	if err != nil {
		t.Fatalf("a tick with one merge waiting on the trash = %v; waiting is "+
			"not a failure", err)
	}
	r.drain()
	if swept["tracker_abandoned_merges"] != 1 {
		t.Errorf("the duty finished %d merges, want 1: %v",
			swept["tracker_abandoned_merges"], swept)
	}
	if b := oneTask(t, r, "b-dup"); b.Merging || b.Status != tracker.StatusCancelled {
		t.Errorf("the merge behind the waiting one did not finish: %q, "+
			"merging=%v", b.Status, b.Merging)
	}
	a := oneTask(t, r, "a-dup")
	if !a.Merging || a.Status == tracker.StatusCancelled {
		t.Errorf("the merge into the trash was finished or cleared (%q, "+
			"merging=%v) — its survivor takes nothing until it is restored",
			a.Status, a.Merging)
	}
	if line := logged.String(); !strings.Contains(line,
		"tracker_merges_waiting_on_the_trash") || !strings.Contains(line, "a-dup") {
		t.Errorf("the waiting merge was not reported: %s", line)
	}

	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "a-keep", "ENG",
		nil); err != nil {
		t.Fatalf("RestoreTask: %v", err)
	}
	r.drain()
	if _, err := worker.Tick(t.Context()); err != nil {
		t.Fatalf("Tick after the restore: %v", err)
	}
	r.drain()
	if a := oneTask(t, r, "a-dup"); a.Merging || a.Status != tracker.StatusCancelled {
		t.Errorf("the merge did not finish once its survivor was restored: "+
			"%q, merging=%v", a.Status, a.Merging)
	}
}

// A MERGE WHOSE SURVIVOR WAS PURGED HAS NO TARGET, AND ITS MARK IS CLEARED.
//
// Nothing can be moved onto, or merged into, a task a purge destroyed — and
// the duty read the survivor off the duplicate's own document, which a purge
// of the other task never rewrites, so it asked the store for a task that
// would never return and was told "not on this node" on every tick. The merge
// did not happen, so the duplicate is left open.
func TestTheDutyClearsAMergeWhoseSurvivorWasPurged(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	markMerging(t, r, "dup", "keep", true)
	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", "keep", "ENG",
		"a test"); err != nil {
		t.Fatalf("PurgeTask: %v", err)
	}
	r.drain()

	if _, err := trackerWorker(t, r).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	r.drain()
	dup := oneTask(t, r, "dup")
	if dup.Merging {
		t.Error("the mark still stands on a merge whose survivor was purged, " +
			"so the duty runs against it for ever")
	}
	if dup.Status == tracker.StatusCancelled {
		t.Error("the duplicate was cancelled into a task a purge destroyed — " +
			"it is now the only copy of the work")
	}
}

// markMerging publishes exactly the mark [tracker.Writer.MergeDuplicates]
// does and nothing after it — a holder that died between its first append and
// its last.
func markMerging(t *testing.T, r *roundTrip, dup, into string, reparent bool) {
	t.Helper()
	merging := true
	if _, err := r.writer.UpdateTask(t.Context(), "op-mark-"+dup, dup, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Relate: &tracker.RelationIntent{Add: []tracker.Relation{{
				Kind: tracker.RelationDuplicates, Other: into,
			}}},
			Merging: &merging, MergeReparent: &reparent,
		}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("mark %s merging into %s: %v", dup, into, err)
	}
	r.drain()
}

// loggedTrackerWorker is [trackerWorker] writing the duty's lines to out.
func loggedTrackerWorker(t *testing.T, r *roundTrip, out *bytes.Buffer) *maintenance.Worker {
	t.Helper()
	w, err := maintenance.New(maintenance.Options{
		Jobs: tracker.Jobs(tracker.DutyDeps{
			DB: r.db, Writer: r.writer, NodeID: "node-a",
			Logger: slog.New(slog.NewTextHandler(out, nil)),
		}),
		Now: func() time.Time { return wednesday },
	})
	if err != nil {
		t.Fatalf("build the tracker's maintenance worker: %v", err)
	}
	return w
}
