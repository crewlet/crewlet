package tracker_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// AN UNBLOCKED DEPENDENT IS FOUND AFTER A GAP OF ANY LENGTH.
//
// # The failure this exists to catch
//
// The repair used to ask for status changes in the last five minutes. A duty
// that did not run for six — a lease flap, a singleton moving, a node restart
// — left every dependent unblocked in that window untold FOR EVER, because
// nothing ever looked further back. A wall-clock window on a repair is a
// repair with a hole exactly the size of the outage it exists to survive.
//
// So the scan is bounded by the position it last read to, and this case is a
// scan from ZERO over records written long before it: any clock-bounded query
// finds nothing here, and a position-bounded one finds the dependent.
func TestAnUnblockedDependentIsFoundAfterAGapOfAnyLength(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker, dependent := newTask("t-1"), newTask("t-2")
	dependent.Relations = []tracker.Relation{{
		Kind: tracker.RelationWaitingOn, Other: "t-1",
	}}
	for _, task := range []tracker.Task{blocker, dependent} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+task.ID, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", task.ID, err)
		}
		r.drain()
	}

	// NOTHING IS OWED WHILE THE BLOCKER IS OPEN.
	if scan := scanUnblocked(t, r, 0); len(scan.Pending) != 0 {
		t.Fatalf("a dependent with an open blocker is owed a wake: %+v",
			scan.Pending)
	}

	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "t-1", "ENG",
		tracker.TaskPatch{Status: &done},
		&tracker.Notify{Kind: tracker.ChangeStatus}); err != nil {
		t.Fatalf("close the blocker: %v", err)
	}
	r.drain()

	// FROM ZERO, which is what a repair that has never run reads and what
	// one returning from an outage of any length reads.
	scan := scanUnblocked(t, r, 0)
	if len(scan.Pending) != 1 || scan.Pending[0].Task != "t-2" {
		t.Fatalf("the scan found %+v, and t-2 is workable and has not been "+
			"told — a bound that cannot reach back past the outage is a "+
			"repair with a hole the size of the outage", scan.Pending)
	}
	if scan.Through == 0 {
		t.Fatal("the scan advanced to no position, so the next tick re-reads " +
			"every record for ever")
	}

	// AND TELLING THEM IS WHAT STOPS IT BEING FOUND AGAIN.
	if _, err := r.writer.TellUnblocked(t.Context(), "op-tell", scan.Pending[0]); err != nil {
		t.Fatalf("TellUnblocked: %v", err)
	}
	r.drain()
	if again := scanUnblocked(t, r, 0); len(again.Pending) != 0 {
		t.Fatalf("the same dependent is owed a second wake: %+v — the stamp "+
			"is a MAX over the commits that named it, so a repair that has "+
			"run must not run again", again.Pending)
	}
}

// A DEPENDENT WITH ANY BLOCKER STILL OPEN IS NOT WORKABLE.
//
// The repair asks "has every blocker cleared", not "has a blocker cleared" —
// and the difference is somebody woken for work they still cannot start.
func TestADependentWithASecondOpenBlockerIsNotTold(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	dependent := newTask("t-3")
	dependent.Relations = []tracker.Relation{
		{Kind: tracker.RelationWaitingOn, Other: "t-1"},
		{Kind: tracker.RelationWaitingOn, Other: "t-2"},
	}
	if _, err := r.writer.CreateTask(t.Context(), "op-t-3", dependent, nil); err != nil {
		t.Fatalf("CreateTask t-3: %v", err)
	}
	r.drain()

	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "t-1", "ENG",
		tracker.TaskPatch{Status: &done},
		&tracker.Notify{Kind: tracker.ChangeStatus}); err != nil {
		t.Fatalf("close one blocker: %v", err)
	}
	r.drain()
	if scan := scanUnblocked(t, r, 0); len(scan.Pending) != 0 {
		t.Fatalf("a dependent with one of two blockers closed was reported "+
			"workable: %+v", scan.Pending)
	}

	if _, err := r.writer.UpdateTask(t.Context(), "op-close-2", "t-2", "ENG",
		tracker.TaskPatch{Status: &done},
		&tracker.Notify{Kind: tracker.ChangeStatus}); err != nil {
		t.Fatalf("close the second blocker: %v", err)
	}
	r.drain()
	if scan := scanUnblocked(t, r, 0); len(scan.Pending) != 1 {
		t.Fatalf("the dependent is workable with both blockers closed and the "+
			"scan found %+v", scan.Pending)
	}
}

func scanUnblocked(t *testing.T, r *roundTrip, since uint64) tracker.UnblockScan {
	t.Helper()
	scan, err := tracker.ScanUnblocked(t.Context(), r.db, since, 64)
	if err != nil {
		t.Fatalf("ScanUnblocked: %v", err)
	}
	return scan
}
