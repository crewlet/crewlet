package tracker_test

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// updatedOf is a task's last-change instant as the COLUMN holds it — what a
// list's Updated column and its "recently updated" order read — beside the
// same instant as the DOCUMENT holds it, which is what a task page reads, and
// the newest `effective_at` among the task's history rows.
func (r *roundTrip) updatedOf(id string) (column, document, newest int64) {
	r.t.Helper()
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		var raw []byte
		if err := tx.QueryRowContext(r.t.Context(),
			`SELECT updated_at, document FROM tracker_tasks WHERE id = ?`, id).
			Scan(&column, &raw); err != nil {
			return err
		}
		var task tracker.Task
		if err := json.Unmarshal(raw, &task); err != nil {
			return err
		}
		document = store.EncodeTime(task.UpdatedAt)
		return tx.QueryRowContext(r.t.Context(),
			`SELECT MAX(effective_at) FROM tracker_history WHERE subject_id = ?`, id).
			Scan(&newest)
	}); err != nil {
		r.t.Fatalf("read task %s's last change: %v", id, err)
	}
	return column, document, newest
}

// A CHANGE MOVES THE TASK'S LAST-CHANGED INSTANT.
//
// Only the create had ever stamped `updated_at`, so a task changed a hundred
// times read as last updated the day it was filed: every list's Updated column
// said so, and "recently updated" ordered the company's work by when it was
// FILED. The stamp is the applier's, at the fleet-agreed instant of the record
// that changed the task — the one its history row carries.
func TestAChangeMovesTheTasksLastChangedInstant(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	filed, _, _ := r.updatedOf("t-1")

	to := tracker.StatusInProgress
	if _, err := r.writer.UpdateTask(t.Context(), "op-2", "t-1", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Status: &to}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()

	column, document, newest := r.updatedOf("t-1")
	if column <= filed {
		t.Errorf("after a status change updated_at is %d, no later than the create's %d", column, filed)
	}
	if column != newest {
		t.Errorf("updated_at is %d and the newest history row is at %d — the list and the "+
			"history disagree about when the task last changed", column, newest)
	}
	if document != column {
		t.Errorf("the document says %d and the column %d — a task page and a list would "+
			"draw two different instants for one change", document, column)
	}
}

// THE BACKFILL EQUALS THE REPLAY, for the last-changed instant as for every
// derived column: rows a build without the stamp wrote re-derive to exactly
// what the incremental rule reached, and rows this build wrote re-derive to
// themselves.
func TestTheLastChangedBackfillEqualsAReplay(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	to := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-done", "t-1", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Status: &to}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()
	replayed := map[string]int64{}
	for _, id := range []string{"t-1", "t-2"} {
		replayed[id], _, _ = r.updatedOf(id)
	}

	rederive := func() int {
		var repaired int
		if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
			n, err := r.applier.Rederive(t.Context(), tx, statelog.ApplyOptions{})
			repaired = n
			return err
		}); err != nil {
			t.Fatalf("re-derive: %v", err)
		}
		return repaired
	}
	if n := rederive(); n != 0 {
		t.Errorf("re-deriving rows this build maintained repaired %d of them", n)
	}

	// THE PREDECESSOR'S ROWS: the stamp as a build without it left it — the
	// create's, on the column and in the document alike.
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE tracker_tasks SET updated_at = 1,
			        document = json_set(document, '$.updated_at', '2001-01-01T00:00:00Z')`)
		return err
	}); err != nil {
		t.Fatalf("age the rows: %v", err)
	}
	if n := rederive(); n != 2 {
		t.Errorf("re-deriving two stale rows repaired %d", n)
	}
	for id, want := range replayed {
		column, document, _ := r.updatedOf(id)
		if column != want || document != want {
			t.Errorf("%s re-derives to %d (document %d) and the replay wrote %d", id, column, document, want)
		}
	}
	if tracker.DerivationVersion < 5 {
		t.Error("the last-changed stamp is a derived column and the derivation version did not move")
	}
}

// A LATE RECORD MOVES THE LAST-CHANGED INSTANT AS A REPLAY WOULD.
//
// A record reprocessed BELOW a task's version changes no document, but its
// history row raises every successor's effective instant — so a node that
// applied the same records in log order holds the raised instant as the
// task's last change. Left on the stamp the task already had, the two nodes
// disagree for good about when it last changed, and the re-derive an upgrade
// runs would "repair" one of them.
func TestALateRecordMovesTheLastChangedInstantAsAReplayWould(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, err := h.applyAt(taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC(), 1); err != nil {
		t.Fatalf("create: %v", err)
	}
	ahead := taskRecord("t-1", tracker.OpPatch, tracker.TaskPatch{Title: ptr("second")}, nil)
	ahead.OpID = "patch-3"
	if _, err := h.applyAt(ahead, time.Unix(1_700_000_200, 0).UTC(), 3); err != nil {
		t.Fatalf("patch: %v", err)
	}
	// Position 2, and a broker instant LATER than position 3's.
	late := taskRecord("t-1", tracker.OpPatch, tracker.TaskPatch{Title: ptr("late")}, nil)
	late.OpID = "patch-2"
	if _, err := h.applyAt(late, time.Unix(1_700_000_900, 0).UTC(), 2); err != nil {
		t.Fatalf("late patch: %v", err)
	}
	newest := h.value(`SELECT MAX(effective_at) FROM tracker_history WHERE subject_id = 't-1'`)
	if want := store.EncodeTime(time.Unix(1_700_000_900, 0).UTC()); newest != want {
		t.Fatalf("the newest history instant is %d, want the late record's raise to %d", newest, want)
	}
	if got := h.value(`SELECT updated_at FROM tracker_tasks WHERE id = 't-1'`); got != newest {
		t.Errorf("updated_at is %d and the newest history row is at %d — a node that applied "+
			"these records in log order holds the raised instant", got, newest)
	}
}
