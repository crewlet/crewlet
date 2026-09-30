package tracker_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE TRASH'S TWO WALKS KEEP THE CONTRACT EVERY OTHER WALK KEEPS.
//
// A subtree removal and its restore are one commit per task, which makes both
// sequences — and they were the two the step contract never reached. Each
// descendant's answer was discarded, so a step whose outcome was unknown was
// carried on over; the root's own unknown was never checked, so descendants
// followed a root nobody could say had gone; and every step was named by its
// POSITION in a list the re-run reads afresh. A restore's list is what is
// still in the trash, so once one task had come back the re-run's first step
// carried the id the first run's first step landed under — on another task —
// and the ledger refused it as an operation id reused: the restore could
// never be finished under its own operation.

// trashFixture is a root with three subtasks, every write applied as it lands.
func trashFixture(t *testing.T) *roundTrip {
	t.Helper()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "tr-root")
	parent := "tr-root"
	for _, id := range []string{"tr-a", "tr-b", "tr-c"} {
		kid := newTask(id)
		kid.Key = "ENG-" + id
		kid.Parent, kid.Depth = &parent, 1
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, kid, nil); err != nil {
			t.Fatalf("file %s: %v", id, err)
		}
		r.drain()
	}
	return r
}

func TestASubtreeRemovalStopsAtAnUnknownStepAndSaysHowFarItGot(t *testing.T) {
	t.Parallel()
	r := trashFixture(t)
	lossy, lost := r.lossyWriter(t)
	op := statelog.NewOpID(time.Now(), "remove")

	lost.dropFor("tr-b")
	_, err := lossy.RemoveTask(t.Context(), op, "tr-root", "ENG", true, nil)
	var stopped *tracker.SubtreeStopped
	if !errors.As(err, &stopped) || !errors.Is(err, tracker.ErrStepUnresolved) {
		t.Fatalf("a removal with an unknown descendant step = %v, want a "+
			"SubtreeStopped wrapping ErrStepUnresolved", err)
	}
	if stopped.Root != "tr-root" || stopped.Followed != 1 || stopped.Of != 3 ||
		!strings.Contains(err.Error(), op) {

		t.Errorf("the stop says %+v (%v), want tr-root with 1 of 3 followed "+
			"under operation %s", *stopped, err, op)
	}
	r.drain()
	if removed(t, r, "tr-c") {
		t.Error("the walk carried on past the unknown step and removed tr-c")
	}

	// THE SAME OPERATION AGAIN FINISHES IT, and what it removes names the
	// root, so a restore of the root brings every one of them back.
	if _, err := lossy.RemoveTask(t.Context(), op, "tr-root", "ENG", true, nil); err != nil {
		t.Fatalf("the re-run of the stopped removal: %v", err)
	}
	r.drain()
	for _, id := range []string{"tr-a", "tr-b", "tr-c"} {
		task := oneTask(t, r, id)
		if task.Removed == nil || task.Removed.RemovedWith == nil ||
			*task.Removed.RemovedWith != "tr-root" {
			t.Errorf("%s after the re-run is %+v, want removed with tr-root", id, task.Removed)
		}
	}
}

// A REMOVAL WHOSE ROOT STEP IS UNKNOWN TAKES NOTHING WITH IT: a subtask removed
// under a root nobody can say went is the orphan the order exists to prevent.
func TestASubtreeRemovalWhoseRootIsUnknownRemovesNothingElse(t *testing.T) {
	t.Parallel()
	r := trashFixture(t)
	lossy, lost := r.lossyWriter(t)

	lost.dropFor("tr-root")
	_, err := lossy.RemoveTask(t.Context(), statelog.NewOpID(time.Now(), "remove"),
		"tr-root", "ENG", true, nil)
	var stopped *tracker.SubtreeStopped
	if !errors.Is(err, tracker.ErrStepUnresolved) || errors.As(err, &stopped) {
		t.Fatalf("a removal with an unknown root = %v, want ErrStepUnresolved and "+
			"no claim that the root landed", err)
	}
	r.drain()
	for _, id := range []string{"tr-a", "tr-b", "tr-c"} {
		if removed(t, r, id) {
			t.Errorf("%s followed a root whose own removal is unknown", id)
		}
	}
}

// A RESTORE STOPPED PART OF THE WAY THROUGH IS FINISHED UNDER ITS OWN
// OPERATION — the case positional step names made impossible — and under a
// new one, on a root that is already back.
func TestARestoreStoppedPartOfTheWayIsFinishedAgain(t *testing.T) {
	t.Parallel()
	for name, sameOp := range map[string]bool{"same operation": true, "new operation": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := trashFixture(t)
			if _, err := r.writer.RemoveTask(t.Context(), "op-rm", "tr-root", "ENG",
				true, nil); err != nil {
				t.Fatalf("remove the subtree: %v", err)
			}
			r.drain()
			lossy, lost := r.lossyWriter(t)
			op := statelog.NewOpID(time.Now(), "restore")

			lost.refuse("tr-b")
			_, err := lossy.RestoreTask(t.Context(), op, "tr-root", "ENG", nil)
			var stopped *tracker.SubtreeStopped
			if !errors.As(err, &stopped) || stopped.Followed != 1 || stopped.Of != 3 {
				t.Fatalf("a restore refused at tr-b = %v, want a SubtreeStopped "+
					"with 1 of 3 followed", err)
			}
			lost.refuse("")
			r.drain()
			if removed(t, r, "tr-root") || removed(t, r, "tr-a") || !removed(t, r, "tr-b") {
				t.Fatal("the premise: the root and tr-a are back and tr-b is not")
			}

			again := statelog.NewOpID(time.Now(), "restore")
			if sameOp {
				again = op
			}
			if _, err := lossy.RestoreTask(t.Context(), again, "tr-root", "ENG", nil); err != nil {
				t.Fatalf("finishing the stopped restore: %v", err)
			}
			r.drain()
			for _, id := range []string{"tr-b", "tr-c"} {
				if removed(t, r, id) {
					t.Errorf("%s is still in the trash after the restore was "+
						"made again", id)
				}
			}
		})
	}
}

// A RESTORE WITH NOTHING TO BRING BACK SAYS SO, and a restore answered from
// its own first copy is not one: that is the success the first copy was.
func TestARestoreWithNothingToBringBackSaysSo(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "tr-live")
	if _, err := r.writer.RemoveTask(t.Context(), "op-rm", "tr-live", "ENG",
		false, nil); err != nil {
		t.Fatalf("remove: %v", err)
	}
	r.drain()
	op := statelog.NewOpID(time.Now(), "restore")
	if _, err := r.writer.RestoreTask(t.Context(), op, "tr-live", "ENG", nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	r.drain()

	if _, err := r.writer.RestoreTask(t.Context(), op, "tr-live", "ENG", nil); err != nil {
		t.Errorf("the restore's own operation again = %v, want the success its "+
			"first copy was", err)
	}
	_, err := r.writer.RestoreTask(t.Context(), statelog.NewOpID(time.Now(), "restore"),
		"tr-live", "ENG", nil)
	if !errors.Is(err, tracker.ErrNothingToRestore) {
		t.Errorf("a new restore of a live task with nothing in the trash with it "+
			"= %v, want ErrNothingToRestore", err)
	}
}
