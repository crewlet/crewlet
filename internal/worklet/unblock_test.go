package worklet_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN UNBLOCKED DEPENDENT IS FOUND AFTER A GAP OF ANY LENGTH.
//
// A wall-clock window on the repair has a hole the size of the outage it must
// survive (a lease flap, a singleton moving, a restart), so the scan is bounded
// by the log position it last read to. This case scans from ZERO over records
// written long before: a clock-bounded query finds nothing, a position-bounded
// one finds the dependent.
func TestAnUnblockedDependentIsFoundAfterAGapOfAnyLength(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker, dependent := newTask("t-1"), newTask("t-2")
	dependent.Relations = []worklet.Relation{{
		Kind: worklet.RelationWaitingOn, Other: "t-1",
	}}
	// THE DEPENDENT HAS AN ASSIGNEE, the notice's only recipient: an
	// unassigned one is owed nothing.
	dependent.Assignee = "alice"
	for _, task := range []worklet.Task{blocker, dependent} {
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

	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "t-1", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Status: &done}, worklet.ChangeStatus,
		&worklet.Notify{Kind: worklet.ChangeStatus}); err != nil {
		t.Fatalf("close the blocker: %v", err)
	}
	r.drain()

	// FROM ZERO, as a repair that has never run, or is back from an outage
	// of any length, reads.
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

// A DEPENDENT WITH ANY BLOCKER STILL OPEN IS NOT WORKABLE: the repair asks
// "has every blocker cleared", or it wakes somebody for work they still cannot
// start.
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
	dependent.Relations = []worklet.Relation{
		{Kind: worklet.RelationWaitingOn, Other: "t-1"},
		{Kind: worklet.RelationWaitingOn, Other: "t-2"},
	}
	// AN ASSIGNEE as scaffolding: the scan skips a dependent with nobody to
	// tell (TestAnUnassignedDependentIsNotToldAboutItself).
	dependent.Assignee = "alice"
	if _, err := r.writer.CreateTask(t.Context(), "op-t-3", dependent, nil); err != nil {
		t.Fatalf("CreateTask t-3: %v", err)
	}
	r.drain()

	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "t-1", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Status: &done}, worklet.ChangeStatus,
		&worklet.Notify{Kind: worklet.ChangeStatus}); err != nil {
		t.Fatalf("close one blocker: %v", err)
	}
	r.drain()
	if scan := scanUnblocked(t, r, 0); len(scan.Pending) != 0 {
		t.Fatalf("a dependent with one of two blockers closed was reported "+
			"workable: %+v", scan.Pending)
	}

	if _, err := r.writer.UpdateTask(t.Context(), "op-close-2", "t-2", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Status: &done}, worklet.ChangeStatus,
		&worklet.Notify{Kind: worklet.ChangeStatus}); err != nil {
		t.Fatalf("close the second blocker: %v", err)
	}
	r.drain()
	if scan := scanUnblocked(t, r, 0); len(scan.Pending) != 1 {
		t.Fatalf("the dependent is workable with both blockers closed and the "+
			"scan found %+v", scan.Pending)
	}
}

func scanUnblocked(t *testing.T, r *roundTrip, since uint64) worklet.UnblockScan {
	t.Helper()
	scan, err := worklet.ScanUnblocked(t.Context(), r.db.Reader(), since, 64)
	if err != nil {
		t.Fatalf("ScanUnblocked: %v", err)
	}
	return scan
}

// AN UNBLOCKED NOTICE NAMES THE TASK BY ITS KEY, NOT BY ITS UUID.
//
// [Snapshot.Key] is read as the ADDRESSABLE name by
// `tracker_notifications.subject_key`, the wake metadata's `item_key` and the
// prompt's "Read **<key>** with get_work_item" block, and this notice is the
// only wake the dependent gets.
func TestAnUnblockedNoticeNamesTheKeyRatherThanTheID(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker, dependent := newTask("t-1"), newTask("t-2")
	dependent.Relations = []worklet.Relation{{
		Kind: worklet.RelationWaitingOn, Other: "t-1",
	}}
	dependent.Assignee = "alice"
	// THE WRITER MINTS THE KEYS, so the case reads them back.
	for _, task := range []worklet.Task{blocker, dependent} {
		task.Key = ""
		if _, err := r.writer.CreateTask(t.Context(), "op-"+task.ID, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", task.ID, err)
		}
		r.drain()
	}
	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &done}, worklet.ChangeStatus,
		&worklet.Notify{Kind: worklet.ChangeStatus}); err != nil {
		t.Fatalf("close the blocker: %v", err)
	}
	r.drain()

	scan := scanUnblocked(t, r, 0)
	if len(scan.Pending) != 1 {
		t.Fatalf("the scan found %+v, want the one dependent", scan.Pending)
	}
	pending := scan.Pending[0]
	want, err := r.reader.Task(t.Context(), "t-2", worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read the dependent back: %v", err)
	}
	if pending.Key != want.Task.Key {
		t.Fatalf("the scan carries key %q, want %q", pending.Key, want.Task.Key)
	}
	if _, err := r.writer.TellUnblocked(t.Context(), "op-tell", pending); err != nil {
		t.Fatalf("TellUnblocked: %v", err)
	}
	wake := r.lastWake()
	if wake == nil {
		t.Fatal("the notice published nothing")
	}
	if wake.Snapshot.Key != want.Task.Key {
		t.Errorf("the notice names %q, want the key %q — a seat told to read "+
			"that by name is told to read a uuid",
			wake.Snapshot.Key, want.Task.Key)
	}
	if wake.Snapshot.Key == want.Task.ID {
		t.Error("the notice put the task's uuid in the KEY field")
	}
}

// AN UNASSIGNED DEPENDENT IS NOT SCANNED, because there is nobody to tell.
//
// The notice's only recipient is the assignee (every other routing arm reads a
// snapshot field this record lacks), so a record for an unassigned task costs
// a routing snapshot on the durable log and a delivery and ack on every node,
// and lands in no inbox. Skipping is free: the window is bounded by log
// position, not the told-stamp, so a skipped dependent is not found again.
func TestAnUnassignedDependentIsNotToldAboutItself(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker := newTask("t-1")
	assigned, unassigned := newTask("t-2"), newTask("t-3")
	assigned.Assignee = "alice"
	for _, task := range []*worklet.Task{&assigned, &unassigned} {
		task.Relations = []worklet.Relation{{
			Kind: worklet.RelationWaitingOn, Other: "t-1",
		}}
	}
	for _, task := range []worklet.Task{blocker, assigned, unassigned} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+task.ID, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", task.ID, err)
		}
		r.drain()
	}
	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &done}, worklet.ChangeStatus,
		&worklet.Notify{Kind: worklet.ChangeStatus}); err != nil {
		t.Fatalf("close the blocker: %v", err)
	}
	r.drain()

	scan := scanUnblocked(t, r, 0)
	var found []string
	for _, u := range scan.Pending {
		found = append(found, u.Task)
	}
	if len(found) != 1 || found[0] != "t-2" {
		t.Fatalf("the scan found %v, want the assigned dependent alone — an "+
			"unassigned one has nobody to tell, and a record for it wakes "+
			"nobody on every node in the company", found)
	}
}

// A DEPENDENT IS FOUND WHEN ITS BLOCKER CLOSED UNDER SOME OTHER WORD.
//
// A row's kind is the word the WRITER chose for a patch that may move several
// things; a QUIET CANCEL filed under another kind still moves the blocker into
// the `done` group and clears every edge naming it, so the dependents are
// workable. Such a record wakes nobody and only [worklet.Writer.TellUnblocked]
// fills `Snapshot.Unblocked`, so this scan is the ONLY way they hear. As
// [Applier.stampStatusEntered] is, the scan is gated on the row's own status
// delta, not its kind; this case uses a kind that is not `status` so a scan
// keyed back on the announced word goes red.
func TestADependentIsFoundWhenItsBlockerClosedUnderAnotherKind(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker, dependent := newTask("t-1"), newTask("t-2")
	dependent.Relations = []worklet.Relation{{
		Kind: worklet.RelationWaitingOn, Other: "t-1",
	}}
	dependent.Assignee = "alice"
	for _, task := range []worklet.Task{blocker, dependent} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+task.ID, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", task.ID, err)
		}
		r.drain()
	}

	// THE BLOCKER IS CANCELLED BY A QUIET WRITE THAT CALLS ITSELF A FIELD
	// EDIT, into the `done` group.
	cancelled := worklet.StatusCancelled
	points := 0.0
	if _, err := r.writer.UpdateTask(t.Context(), "op-cancel", "t-1", "ENG",
		worklet.NoIfMatch,
		worklet.TaskPatch{Status: &cancelled, Points: &points},
		worklet.ChangeFields, nil); err != nil {
		t.Fatalf("cancel the blocker through a field edit: %v", err)
	}
	r.drain()

	scan := scanUnblocked(t, r, 0)
	if len(scan.Pending) != 1 || scan.Pending[0].Task != "t-2" {
		t.Fatalf("the scan found %+v, and t-2 is workable — its blocker was "+
			"cancelled into a finished group and every edge naming it was "+
			"cleared. A scan keyed on the ANNOUNCED word misses it, and "+
			"nothing else ever tells this person", scan.Pending)
	}
	// AND THE HORIZON COVERS ONLY WHAT THE SCAN LOOKED AT: it shares the
	// rows' predicate, since a horizon over a wider set steps past records
	// the rows predicate never saw, and they are never reconsidered.
	if scan.Through == 0 {
		t.Fatal("the scan advanced to no position, so the next tick re-reads " +
			"every record for ever")
	}
	// RE-READ FROM ZERO: the dependent is still owed until somebody tells
	// them.
	if again := scanUnblocked(t, r, 0); len(again.Pending) != 1 {
		t.Errorf("a second scan from zero found %+v, want the same dependent "+
			"still owed — it has not been told yet", again.Pending)
	}
}

// AND THE INDEXES NOTHING READS ARE GONE: an index costs a write on every
// applied commit. This repair no longer selects on `kind`, and no query filters
// or orders on `effective_at`.
func TestTheDeadHistoryIndexesAreGone(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, dead := range []string{
		"tracker_history_kind_seq_idx", "tracker_history_kind_idx",
	} {
		got := r.strings(`SELECT name FROM sqlite_master
			WHERE type = 'index' AND name = ?`, dead)
		if len(got) != 0 {
			t.Errorf("%s is still maintained on every applied commit, and "+
				"nothing reads it", dead)
		}
	}
	// AND THE ONE THAT REPLACED THEM IS THERE: a swap, not a deletion.
	if got := r.strings(`SELECT name FROM sqlite_master
		WHERE type = 'index' AND name = 'tracker_history_status_seq_idx'`); len(got) != 1 {
		t.Error("the repair's own index is absent, so the scan it serves " +
			"reads the whole table")
	}
}
