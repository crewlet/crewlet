package tracker_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
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
	// THE DEPENDENT HAS AN ASSIGNEE, because the notice's only recipient
	// IS the assignee — an unassigned dependent is owed nothing, and a
	// fixture without one would assert a wake with nobody to receive it.
	dependent.Assignee = "alice"
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
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "t-1", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Status: &done}, tracker.ChangeStatus,
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
	// AN ASSIGNEE, because the notice has no other recipient and the scan
	// skips a dependent with nobody to tell. This case is about the
	// EVERY-BLOCKER rule, so the assignment is scaffolding rather than
	// what it asserts — see TestAnUnassignedDependentIsNotToldAboutItself.
	dependent.Assignee = "alice"
	if _, err := r.writer.CreateTask(t.Context(), "op-t-3", dependent, nil); err != nil {
		t.Fatalf("CreateTask t-3: %v", err)
	}
	r.drain()

	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "t-1", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Status: &done}, tracker.ChangeStatus,
		&tracker.Notify{Kind: tracker.ChangeStatus}); err != nil {
		t.Fatalf("close one blocker: %v", err)
	}
	r.drain()
	if scan := scanUnblocked(t, r, 0); len(scan.Pending) != 0 {
		t.Fatalf("a dependent with one of two blockers closed was reported "+
			"workable: %+v", scan.Pending)
	}

	if _, err := r.writer.UpdateTask(t.Context(), "op-close-2", "t-2", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Status: &done}, tracker.ChangeStatus,
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

// AN UNBLOCKED NOTICE NAMES THE TASK BY ITS KEY, NOT BY ITS UUID.
//
// [Snapshot.Key] is read as the ADDRESSABLE name by three different things —
// `tracker_notifications.subject_key`, the wake metadata's `item_key`, and the
// prompt's "Read **<key>** with get_work_item" block — and the scan used to
// put the task's uuid in it. The notice is the ONLY wake the blocker side
// ever gets, so it told the one person who needed it to go and fetch
// `3f2a…` by name, and left the one row in that column that is not a key.
func TestAnUnblockedNoticeNamesTheKeyRatherThanTheID(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker, dependent := newTask("t-1"), newTask("t-2")
	dependent.Relations = []tracker.Relation{{
		Kind: tracker.RelationWaitingOn, Other: "t-1",
	}}
	dependent.Assignee = "alice"
	// THE KEYS ARE MINTED BY THE WRITER, so the case reads them back
	// rather than asserting against a literal it chose.
	for _, task := range []tracker.Task{blocker, dependent} {
		task.Key = ""
		if _, err := r.writer.CreateTask(t.Context(), "op-"+task.ID, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", task.ID, err)
		}
		r.drain()
	}
	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus,
		&tracker.Notify{Kind: tracker.ChangeStatus}); err != nil {
		t.Fatalf("close the blocker: %v", err)
	}
	r.drain()

	scan := scanUnblocked(t, r, 0)
	if len(scan.Pending) != 1 {
		t.Fatalf("the scan found %+v, want the one dependent", scan.Pending)
	}
	pending := scan.Pending[0]
	want, err := r.reader.Task(t.Context(), "t-2", tracker.DetailWants{},
		statelog.ReadSession)
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
// The notice's only recipient is the dependent's own assignee — every other
// routing arm reads a snapshot field this record does not carry — so a repair
// record for an unassigned task names NOBODY, and the duty manufactured one on
// a timer for every unassigned dependent that became workable: a full routing
// snapshot on the durable log, a change-feed delivery and an ack for every
// node in the company, a parse, and no inbox row at the end of it.
//
// Skipping costs nothing: the scan's window is bounded by the log position
// rather than by the told-stamp, so a skipped dependent is never found again
// rather than found repeatedly.
func TestAnUnassignedDependentIsNotToldAboutItself(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker := newTask("t-1")
	assigned, unassigned := newTask("t-2"), newTask("t-3")
	assigned.Assignee = "alice"
	for _, task := range []*tracker.Task{&assigned, &unassigned} {
		task.Relations = []tracker.Relation{{
			Kind: tracker.RelationWaitingOn, Other: "t-1",
		}}
	}
	for _, task := range []tracker.Task{blocker, assigned, unassigned} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+task.ID, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", task.ID, err)
		}
		r.drain()
	}
	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus,
		&tracker.Notify{Kind: tracker.ChangeStatus}); err != nil {
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
// # The failure this exists to catch
//
// The scan selected `kind = 'status'`, which is the word the WRITER chose for
// a patch that may have moved several things — not what the row records. The
// reachable case is the sprint ROLLOVER CLOSE: it cancels every straggler in
// the sprint it is emptying, and `StatusCancelled` is in the `done` group,
// whose own description names this path. So the apply stamps the task
// finished and clears every dependency edge naming it a blocker — the
// dependents are workable — while the record itself is about the sprint.
//
// Two things made it permanent rather than merely late. The record is quiet by
// design ("a rollover is ONE thing that happened"), and nothing outside
// [tracker.Writer.TellUnblocked] ever fills `Snapshot.Unblocked`, so this scan
// is the ONLY path by which those people hear. And the horizon was computed
// from the same predicate, so it advanced past the record on the next status
// row anywhere in the log and never reconsidered it: the file's own promise
// that "a gap of any length is caught up on the next tick" did not hold for a
// row the predicate could not name.
//
// The fix is [Applier.recomputeSpans]' — the row's own status delta decides,
// not its kind — and this case is written against a kind that is not `status`
// so that a scan keyed back on the announced word goes red.
func TestADependentIsFoundWhenItsBlockerClosedUnderAnotherKind(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker, dependent := newTask("t-1"), newTask("t-2")
	dependent.Relations = []tracker.Relation{{
		Kind: tracker.RelationWaitingOn, Other: "t-1",
	}}
	dependent.Assignee = "alice"
	for _, task := range []tracker.Task{blocker, dependent} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+task.ID, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", task.ID, err)
		}
		r.drain()
	}

	// THE BLOCKER IS CANCELLED BY A WRITE THAT CALLS ITSELF A SPRINT
	// MOVE, which is exactly the shape the rollover close publishes:
	// quiet, a status into the `done` group, and a kind that is not
	// `status`.
	cancelled := tracker.StatusCancelled
	clear := 0
	if _, err := r.writer.UpdateTask(t.Context(), "op-rollover", "t-1", "ENG",
		tracker.NoIfMatch,
		tracker.TaskPatch{Status: &cancelled, Sprint: &clear},
		tracker.ChangeSprint, nil); err != nil {
		t.Fatalf("cancel the blocker through a sprint move: %v", err)
	}
	r.drain()

	scan := scanUnblocked(t, r, 0)
	if len(scan.Pending) != 1 || scan.Pending[0].Task != "t-2" {
		t.Fatalf("the scan found %+v, and t-2 is workable — its blocker was "+
			"cancelled into a finished group and every edge naming it was "+
			"cleared. A scan keyed on the ANNOUNCED word misses it, and "+
			"nothing else ever tells this person", scan.Pending)
	}
	// AND THE HORIZON COVERS ONLY WHAT THE SCAN LOOKED AT. It is computed
	// from the SAME predicate as the rows, which is what makes advancing
	// it safe: a horizon over a wider set steps past records the rows
	// predicate never saw, and that is what made this permanent rather
	// than merely late.
	if scan.Through == 0 {
		t.Fatal("the scan advanced to no position, so the next tick re-reads " +
			"every record for ever")
	}
	// RE-READ FROM ZERO, which is what a repair that has never run reads
	// and what one returning from an outage of any length reads: the
	// dependent is still owed, because nobody has told them.
	if again := scanUnblocked(t, r, 0); len(again.Pending) != 1 {
		t.Errorf("a second scan from zero found %+v, want the same dependent "+
			"still owed — it has not been told yet", again.Pending)
	}
}

// THE REPAIR'S SCAN IS SERVED BY AN INDEX, not by reading the whole table.
//
// `tracker_history` grows for the life of the company and is never swept — it
// is the account of everything that ever happened — and this scan runs on
// every tick of the tracker duty. A predicate with no index behind it is a
// full scan of that table, forever, several times a minute.
//
// The index it used to have named `kind`, and the scan stopped selecting on
// `kind` when it moved to the row's own status delta (see
// [TestADependentIsFoundWhenItsBlockerClosedUnderAnotherKind]). Migration
// 0008 replaces it with a PARTIAL index over exactly the new predicate, which
// is the only reason that move is affordable.
//
// A TRIPWIRE ON THE SCHEMA AND THE PLANNER, in `storetest`'s own idiom: it
// re-states the predicate rather than borrowing the query's string, so a
// rewrite of the scan that quietly stopped matching the index still passes
// here and is caught by the behavioural cases above plus this plan going
// unused.
func TestTheUnblockedScanIsIndexServed(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	var plan string
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `
			EXPLAIN QUERY PLAN
			SELECT COALESCE(MAX(log_seq), 0) FROM tracker_history
			WHERE log_seq > 0
			  AND json_extract(fields_json, '$.status.to') IS NOT NULL`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var a, b, c int
			var detail string
			if err := rows.Scan(&a, &b, &c, &detail); err != nil {
				return err
			}
			plan += detail + " "
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("explain the repair's horizon: %v", err)
	}
	if !strings.Contains(plan, "tracker_history_status_seq_idx") {
		t.Errorf("the repair's horizon is planned as %q — it names no index, "+
			"so every tick reads the whole of tracker_history, which is the "+
			"account of everything that ever happened and is never swept",
			plan)
	}
	if strings.Contains(plan, "SCAN tracker_history") {
		t.Errorf("the repair's horizon SCANS tracker_history: %q", plan)
	}
}

// AND THE INDEX THE SCAN NO LONGER USES IS GONE.
//
// An index is maintained on every applied commit. One with no query behind it
// is the write cost of a reader that does not exist — and both of these named
// their reader in a comment, which is what made them look alive:
// `tracker_history_kind_seq_idx` named this repair, which stopped selecting on
// `kind`, and `tracker_history_kind_idx` named "every report's window
// predicate" when no query in the tree filters or orders on `effective_at` at
// all. The reports read `tracker_status_spans`, which the applier derives FROM
// this table.
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
	// AND THE ONE THAT REPLACED THEM IS THERE, so this asserts a swap
	// rather than a deletion.
	if got := r.strings(`SELECT name FROM sqlite_master
		WHERE type = 'index' AND name = 'tracker_history_status_seq_idx'`); len(got) != 1 {
		t.Error("the repair's own index is absent, so the scan it serves " +
			"reads the whole table")
	}
}
