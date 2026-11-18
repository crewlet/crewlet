package worklet_test

import (
	"database/sql"
	"testing"

	"github.com/crewlet/crewlet/internal/worklet"
)

// AN ANNOUNCED CHANGE THAT NAMES NOBODY IS STILL `notified`.
//
// `notified` is the ANNOUNCEMENT distinction, not "somebody was told": who was
// told is [Route]'s answer, which needs the CURRENT roster the applier
// deliberately does not hold (two nodes on different epochs would write
// different `tracker_history` rows for one record). A Notify that routes to
// nobody still writes notified=1 and zero notification rows.
func TestAnAnnouncedChangeThatNamesNobodyIsStillNotified(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("A task")

	// A NOTIFICATION NAMING NOBODY: every routing field empty, as for an
	// unassigned task whose only watcher just left.
	empty := &worklet.Notify{Kind: worklet.ChangeFields}
	if got := worklet.Candidates(empty); len(got) != 0 {
		t.Fatalf("the fixture names %v, so it does not test what it says", got)
	}
	title := "Renamed"
	if _, err := r.writer.UpdateTask(t.Context(), "op-silent", task.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Title: &title}, worklet.ChangeFields, empty); err != nil {

		t.Fatalf("update: %v", err)
	}
	r.drain()

	feed := r.activity(worklet.ActivityQuery{Task: task.ID})
	if len(feed.Records) == 0 {
		t.Fatal("the change wrote no history row at all")
	}
	newest := feed.Records[0]
	if !newest.Notified {
		t.Error("a change that carried a notification is recorded as having " +
			"announced nothing — `notified` is the ANNOUNCEMENT distinction, " +
			"not a claim that somebody was woken")
	}
	// AND NOBODY WAS TOLD: a different fact from the row's.
	if n := r.notificationRows(newest.ID); n != 0 {
		t.Errorf("%d notification rows for a change that named nobody", n)
	}
}

// notificationRows counts what the applier wrote for one record.
func (r *roundTrip) notificationRows(recordID string) int {
	r.t.Helper()
	var n int
	if err := r.db.Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(),
			`SELECT COUNT(*) FROM tracker_notifications WHERE record_id = ?`,
			recordID).Scan(&n)
	}); err != nil {
		r.t.Fatalf("count the notification rows: %v", err)
	}
	return n
}
