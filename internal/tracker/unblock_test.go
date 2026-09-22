package tracker_test

import (
	"database/sql"
	"fmt"
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
// reachable case is a BULK CANCEL: `StatusCancelled` is in the `done` group,
// whose own description names this path. So the apply stamps each task
// finished and clears every dependency edge naming it a blocker — the
// dependents are workable — while the record itself announces whatever else
// the patch moved.
//
// Two things made it permanent rather than merely late. Such a record is quiet
// by design, and nothing outside [tracker.Writer.TellUnblocked] ever fills
// `Snapshot.Unblocked`, so this scan is the ONLY path by which those people
// hear. And the horizon was computed
// from the same predicate, so it advanced past the record on the next status
// row anywhere in the log and never reconsidered it: the file's own promise
// that "a gap of any length is caught up on the next tick" did not hold for a
// row the predicate could not name.
//
// The fix is the one [Applier.stampStatusEntered] is gated by — the row's own
// status delta decides, not its kind — and this case is written against a kind
// that is not `status`
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

	// THE BLOCKER IS CANCELLED BY A WRITE THAT CALLS ITSELF A FIELD EDIT,
	// which is exactly the shape a bulk cancel publishes: quiet, a status
	// into the `done` group, and a kind that is not `status`.
	cancelled := tracker.StatusCancelled
	points := 0.0
	if _, err := r.writer.UpdateTask(t.Context(), "op-cancel", "t-1", "ENG",
		tracker.NoIfMatch,
		tracker.TaskPatch{Status: &cancelled, Points: &points},
		tracker.ChangeFields, nil); err != nil {
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
// all.
//
// A COMMENT NAMING A READER IS A CLAIM. The same failure outlived these two:
// `tracker_status_spans_group_idx` named cycle time and lead time, neither of
// which has ever existed here, and migration 0013 kept a whole table on the
// strength of it before 0014 checked and dropped it.
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

// EVERY DEPENDENT PAST THE SCAN'S LIMIT IS EVENTUALLY TOLD.
//
// # The failure this exists to catch
//
// The horizon was a MAX over every status-carrying row in the window,
// computed with no reference to the limit, while the rows below it were cut
// at `ORDER BY t.id LIMIT ?`. One blocker clearing more dependents than the
// limit therefore produced a tick that told the first n, advanced the position
// past the record that made all of them workable, and never looked at the rest
// again: the notice is the ONLY wake the dependents of a quietly-cleared
// blocker ever get, so those people were silently never told, for ever.
//
// This case is the duty's own loop — scan, tell, carry the position forward —
// with a limit deliberately smaller than the dependent set. It is written
// against the OUTCOME rather than against the horizon so that it stays true of
// any fix: whatever the scan does with its position, everybody workable ends
// up told.
func TestEveryDependentPastTheScansLimitIsEventuallyTold(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// SMALLER THAN THE DEPENDENT SET, which is the whole case. The duty
	// passes [tracker.WalkBatch]; the arithmetic is the same at 2 and 64,
	// and at 2 the fixture is five tasks rather than sixty-five.
	const limit, dependents = 2, 5

	blocker := newTask("t-0")
	if _, err := r.writer.CreateTask(t.Context(), "op-t-0", blocker, nil); err != nil {
		t.Fatalf("CreateTask t-0: %v", err)
	}
	r.drain()
	owed := map[string]bool{}
	for i := 1; i <= dependents; i++ {
		id := fmt.Sprintf("d-%d", i)
		dependent := newTask(id)
		// AN ASSIGNEE EACH, because the notice has no other recipient
		// and the scan skips a dependent with nobody to tell.
		dependent.Assignee = "alice"
		dependent.Relations = []tracker.Relation{{
			Kind: tracker.RelationWaitingOn, Other: "t-0",
		}}
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, dependent, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		// DRAINED BETWEEN THE CREATES: they mint keys from one project
		// counter, and a create cannot decide against a number this
		// node has not applied.
		r.drain()
		owed[id] = true
	}

	// ONE CLOSE MAKES ALL FIVE WORKABLE, which is what puts more
	// dependents behind a single log record than one tick may carry.
	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "t-0", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus,
		&tracker.Notify{Kind: tracker.ChangeStatus}); err != nil {
		t.Fatalf("close the blocker: %v", err)
	}
	r.drain()

	// THE DUTY'S OWN LOOP. The bound is what a wedged repair fails
	// against: five dependents at two a tick drain in three, and the
	// fourth is the tick that finds nothing left.
	var since uint64
	for tick := range dependents + 2 {
		scan, err := tracker.ScanUnblocked(t.Context(), r.db, since, limit)
		if err != nil {
			t.Fatalf("ScanUnblocked at tick %d: %v", tick, err)
		}
		if len(scan.Pending) > limit {
			t.Fatalf("tick %d carried %d dependents over a limit of %d — the "+
				"probe row reached the caller and became a notice",
				tick, len(scan.Pending), limit)
		}
		// A TRUNCATED TICK MUST NOT MOVE THE POSITION PAST WHAT IT
		// COVERED. This is the defect itself: the rows the scan did not
		// carry are below the horizon, so advancing over them is the
		// moment those dependents stop being reachable.
		if scan.Truncated && scan.Through != since {
			t.Fatalf("tick %d carried %d of the dependents behind one record "+
				"and still advanced the position from %d to %d — everybody it "+
				"did not carry is below that horizon and is never looked at "+
				"again", tick, len(scan.Pending), since, scan.Through)
		}
		for _, pending := range scan.Pending {
			if _, err := r.writer.TellUnblocked(t.Context(),
				fmt.Sprintf("op-tell-%s-%d", pending.Task, tick), pending); err != nil {
				t.Fatalf("TellUnblocked %s: %v", pending.Task, err)
			}
			r.drain()
			delete(owed, pending.Task)
		}
		since = scan.Through
		if len(scan.Pending) == 0 {
			break
		}
	}
	if len(owed) != 0 {
		t.Fatalf("%d dependents were never told they are workable: %v — their "+
			"blocker is closed, the late notice is the only wake they ever "+
			"get, and nothing looks at them again", len(owed), owed)
	}
}

// A SCAN THAT EXACTLY FILLS ITS LIMIT IS NOT A TRUNCATED SCAN.
//
// ASKED, NOT INFERRED. `len(Pending) == limit` is not "there is more": a
// window holding exactly the limit holds everything it has, and reading the
// full page as evidence of a cut pins the repair's position on a window it has
// already drained — so the same range is re-read on every tick for ever, and
// the log record that would move it on is never crossed.
//
// The probe row is what tells the two apart: the query asks for one row past
// the bound, the extra row is dropped rather than carried, and its PRESENCE is
// the flag.
func TestAScanThatExactlyFillsItsLimitStillAdvances(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	const limit = 2

	blocker := newTask("t-0")
	if _, err := r.writer.CreateTask(t.Context(), "op-t-0", blocker, nil); err != nil {
		t.Fatalf("CreateTask t-0: %v", err)
	}
	r.drain()
	// EXACTLY THE LIMIT, which is the boundary the probe row buys.
	for i := 1; i <= limit; i++ {
		id := fmt.Sprintf("d-%d", i)
		dependent := newTask(id)
		dependent.Assignee = "alice"
		dependent.Relations = []tracker.Relation{{
			Kind: tracker.RelationWaitingOn, Other: "t-0",
		}}
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, dependent, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "t-0", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus,
		&tracker.Notify{Kind: tracker.ChangeStatus}); err != nil {
		t.Fatalf("close the blocker: %v", err)
	}
	r.drain()

	// THE SCAN'S OWN LIMIT, not the 64 the shared helper passes: what this
	// case is about is the boundary at the limit, and a scan reading far
	// under its bound never reaches it.
	scan, err := tracker.ScanUnblocked(t.Context(), r.db, 0, limit)
	if err != nil {
		t.Fatalf("ScanUnblocked: %v", err)
	}
	if len(scan.Pending) != limit {
		t.Fatalf("the scan found %+v, want both dependents", scan.Pending)
	}
	if scan.Truncated {
		t.Error("a window holding exactly the limit reported itself cut — " +
			"inferring the cut from a full page pins the position on a " +
			"window that is already drained")
	}
	if scan.Through == 0 {
		t.Fatal("the scan advanced to no position, so the next tick re-reads " +
			"every record for ever")
	}
}

// A SCAN THAT MAY CARRY NOBODY IS REFUSED, NAMING THE LIMIT.
//
// The bound is one row past the limit and the extra row is evidence rather
// than an answer, so at a limit below one the query returns nothing a caller
// may keep and every scan reports itself cut: nobody is ever told, the
// position never advances, and the repair reports success on every tick while
// doing nothing at all. There is no value to fall back to that would not be
// somebody else's guess at the pacing, so the scan refuses and names the
// parameter to change.
func TestAScanWithNoRoomForADependentIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, limit := range []int{0, -1} {
		_, err := tracker.ScanUnblocked(t.Context(), r.db, 0, limit)
		if err == nil {
			t.Fatalf("a scan with limit %d was accepted, and it can carry no "+
				"dependent and advance no position", limit)
		}
		if !strings.Contains(err.Error(), "limit") {
			t.Errorf("the refusal for limit %d is %q and does not name the "+
				"parameter a caller has to change", limit, err)
		}
	}
}
