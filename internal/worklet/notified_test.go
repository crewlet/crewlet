package worklet_test

import (
	"database/sql"
	"testing"

	"github.com/crewlet/crewlet/internal/worklet"
)

// AN ANNOUNCED CHANGE THAT NAMES NOBODY IS STILL `notified`.
//
// This is the assertion whose absence let four doc comments drift to the
// stronger claim — "whether anybody was told", "a commit that woke nobody",
// and the dashboard's own "(quiet)" label. The column does not mean that and
// the applier could not answer it: who was actually told is [Route]'s answer,
// and Route needs the company's CURRENT roster, which the applier deliberately
// does not hold — two nodes briefly on different epochs would then write
// different rows for one record, and `tracker_history` claims identity.
//
// So `notified` is the ANNOUNCEMENT distinction, exactly as the schema comment
// and the spec define it, and this pins the direction somebody would otherwise
// "fix": a record carrying a Notify that routes to nobody still writes
// notified=1, and writes zero notification rows.
func TestAnAnnouncedChangeThatNamesNobodyIsStillNotified(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("A task")

	// A NOTIFICATION NAMING NOBODY. Every routing arm reads a snapshot
	// field, and this one leaves all of them empty — which is reachable in
	// production: an unwatched, unassigned task whose only watcher just
	// removed themselves.
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
	// AND NOBODY WAS TOLD, which is the other half: the two facts are
	// different and the row records the first one.
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
