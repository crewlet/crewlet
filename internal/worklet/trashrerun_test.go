package worklet_test

import (
	"database/sql"
	"testing"
)

// underRoot files a root and three children under it, t-a, t-b and t-c.
func underRoot(t *testing.T, r *roundTrip) {
	t.Helper()
	if _, err := r.writer.CreateTask(t.Context(), "op-root", newTask("t-root"), nil); err != nil {
		t.Fatalf("CreateTask root: %v", err)
	}
	r.drain()
	parent := "t-root"
	for _, id := range []string{"t-a", "t-b", "t-c"} {
		kid := newTask(id)
		kid.Parent, kid.Depth = &parent, 1
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, kid, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
}

// clearDeferred takes back what [roundTrip.deferRecordOn] wrote: the node
// catching up with the newer build whose record it set aside.
func (r *roundTrip) clearDeferred() {
	r.t.Helper()
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.t.Context(),
			`DELETE FROM tracker_log_deferred_scope`); err != nil {
			return err
		}
		_, err := tx.ExecContext(r.t.Context(), `DELETE FROM tracker_log_deferred`)
		return err
	}); err != nil {
		r.t.Fatalf("clear the deferred record: %v", err)
	}
}

// A RESTORE THAT HALF-FINISHED RESTORES THE REST WHEN IT IS RE-RUN, as its
// error promises: the re-run's list is what is STILL in the trash, so a step
// named by its place in that list would read another task's ledger row and
// leave its own task in the trash under a restored parent.
func TestARestoreThatHalfFinishedRestoresTheRestOnARerun(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	underRoot(t, r)
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-root", "ENG",
		true, nil); err != nil {
		t.Fatalf("RemoveTask subtree: %v", err)
	}
	r.drain()

	// THE FIRST RUN STOPS AT t-b, whose scope holds a deferred newer record.
	r.deferRecordOn("t-b", "ENG")
	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "t-root",
		"ENG", false, nil); err == nil {
		t.Fatal("the restore finished through a task this node cannot write")
	}
	r.drain()
	if removed(t, r, "t-a") || !removed(t, r, "t-b") {
		t.Fatal("the first run did not stop between t-a and t-b, so this case " +
			"is not about a re-run at all")
	}

	r.clearDeferred()
	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "t-root",
		"ENG", false, nil); err != nil {
		t.Fatalf("the re-run of the same restore: %v", err)
	}
	r.drain()
	for _, id := range []string{"t-root", "t-a", "t-b", "t-c"} {
		if removed(t, r, id) {
			t.Errorf("%s is still in the trash after the restore was re-run "+
				"to completion", id)
		}
	}
}

// A REMOVAL THAT HALF-FINISHED REMOVES THE REST WHEN IT IS RE-RUN, even when a
// task it already removed was purged in between: the purge shifts every later
// descendant up a place in the walk, so a step named by place would read the
// purged task's ledger row and leave a live task under a removed parent.
func TestARemovalThatHalfFinishedRemovesTheRestOnARerun(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	underRoot(t, r)

	r.deferRecordOn("t-b", "ENG")
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-root", "ENG",
		true, nil); err == nil {
		t.Fatal("the removal finished through a task this node cannot write")
	}
	r.drain()
	if !removed(t, r, "t-a") || removed(t, r, "t-b") {
		t.Fatal("the first run did not stop between t-a and t-b, so this case " +
			"is not about a re-run at all")
	}
	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", "t-a", "ENG",
		"filed twice"); err != nil {
		t.Fatalf("PurgeTask: %v", err)
	}
	r.drain()

	r.clearDeferred()
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-root", "ENG",
		true, nil); err != nil {
		t.Fatalf("the re-run of the same removal: %v", err)
	}
	r.drain()
	for _, id := range []string{"t-root", "t-b", "t-c"} {
		if !removed(t, r, id) {
			t.Errorf("%s is live after the removal was re-run to completion", id)
		}
	}
}
