package worklet_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A COMMITTED BATCH SAYS WHOSE INBOX MOVED — once per person, only after the
// commit, and never for a notice it did not write. It is how a Crewlet-only
// person learns they have work, announced by the applier on every node to its
// own sockets, never by the writer's goroutine:
//
//   - AFTER THE COMMIT: announced inside the transaction, a body that failed
//     and re-ran would announce a notice no row holds.
//   - ONLY WHAT WAS WRITTEN: a redelivery writes no inbox row, and announcing
//     it would refresh every watcher's screen for nothing.
//   - ONE PER HANDLE PER BATCH, carrying the newest subject: an import filing
//     a hundred tasks for one person is one push, not a hundred.
func TestACommittedBatchSaysWhoseInboxMoved(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	var got [][]inbox.Movement
	h.applier = worklet.NewApplier(func(m []inbox.Movement) {
		got = append(got, m)
	})

	// A QUIET CREATE concerns nobody.
	if _, err := h.apply(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	h.applier.Committed(context.Background())
	if len(got) != 0 {
		t.Fatalf("a quiet commit moved %v", got)
	}

	loud := taskRecord("t-1", worklet.OpPatch, worklet.TaskPatch{Title: ptr("louder")},
		&worklet.Notify{
			Kind: worklet.ChangeStatus,
			Snapshot: worklet.Snapshot{
				Key: "ENG-1", Assignee: "bo", Watchers: []string{"cy"},
			},
		})
	if _, err := h.apply(loud, time.Unix(1_700_000_200, 0).UTC()); err != nil {
		t.Fatalf("patch: %v", err)
	}
	// NOTHING BEFORE THE COMMIT'S OWN HALF, which the framework calls after
	// the transaction lands.
	if len(got) != 0 {
		t.Fatalf("a movement was announced before the commit: %v", got)
	}
	h.applier.Committed(context.Background())
	// EACH CARRIES THE BROKER'S INSTANT for the record that moved it, which
	// the socket's push is timed from.
	stored := time.Unix(1_700_000_200, 0).UTC()
	want := []inbox.Movement{
		moved("bo", 1, "t-1", worklet.ReasonAssignee, stored),
		moved("cy", 1, "t-1", worklet.ReasonWatcher, stored),
	}
	if len(got) != 1 || !slices.Equal(got[0], want) {
		t.Fatalf("the loud commit announced %v, want %v", got, want)
	}

	// A REDELIVERY writes no row and moves nobody.
	got = nil
	if _, err := h.applyAt(loud, time.Unix(1_700_000_200, 0).UTC(), h.seq); err != nil {
		t.Fatalf("redeliver: %v", err)
	}
	h.applier.Committed(context.Background())
	if len(got) != 0 {
		t.Errorf("a redelivery that wrote no inbox row announced %v", got)
	}
	// AND ONE THAT GETS PAST THE HISTORY ROW (reanchored away, its notices
	// kept) still writes no notice and moves nobody: what is announced is
	// what the inbox rows gained, never what a record could have said.
	if err := h.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`DELETE FROM tracker_history WHERE id = 't-1-patch'`)
		return err
	}); err != nil {
		t.Fatalf("reanchor the history row away: %v", err)
	}
	if _, err := h.applyAt(loud, time.Unix(1_700_000_200, 0).UTC(), h.seq); err != nil {
		t.Fatalf("reapply: %v", err)
	}
	h.applier.Committed(context.Background())
	if len(got) != 0 {
		t.Errorf("a reapply whose notices were already written announced %v", got)
	}

	// TWO NOTICES FOR ONE PERSON IN ONE BATCH are one movement of two,
	// naming the newer and timed from the OLDER: one push answers both, and
	// the person waited longest for the first.
	second := taskRecord("t-2", worklet.OpCreate, newTask("t-2"), &worklet.Notify{
		Kind: worklet.ChangeCreated, Snapshot: worklet.Snapshot{Key: "ENG-2", Assignee: "bo"},
	})
	second.Subject = worklet.TaskSubject("t-2")
	third := taskRecord("t-3", worklet.OpCreate, newTask("t-3"), &worklet.Notify{
		Kind: worklet.ChangeCreated, Snapshot: worklet.Snapshot{Key: "ENG-3", Assignee: "bo"},
	})
	third.Subject = worklet.TaskSubject("t-3")
	for i, rec := range []worklet.MutationRecord{second, third} {
		if _, err := h.apply(rec, time.Unix(1_700_000_300+int64(i)*50, 0).UTC()); err != nil {
			t.Fatalf("create %s: %v", rec.Subject.ID, err)
		}
	}
	h.applier.Committed(context.Background())
	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("one batch for one person announced %v, want one movement", got)
	}
	source := worklet.Domain{}.Name()
	if m := got[0][0]; m.Source != source || m.Handle != "bo" ||
		m.UnreadDelta != 2 || m.Subject != "t-3" ||
		!m.At.Equal(time.Unix(1_700_000_300, 0)) {
		t.Errorf("the batch announced %+v, want bo moved by 2 with t-3 newest, timed "+
			"from t-2's instant", m)
	}
}

// moved is one movement Worklet's applier announces, under its state log's
// name (what a node relays it by and a screen reads as which half of its
// inbox moved), timed from at, the broker's instant.
func moved(handle string, unread int, subject string, reason worklet.Reason,
	at time.Time) inbox.Movement {
	return inbox.Movement{Source: worklet.Domain{}.Name(), Handle: handle,
		UnreadDelta: unread, Subject: subject, Reason: string(reason), At: at}
}

// A PURGE SAYS WHOSE INBOX IT EMPTIED, as surely as a commit says whose it
// filled: it deletes the notices its task's records routed (they hold
// excerpts), and an inbox not told keeps listing notices no node holds until
// its next poll. The recipients of what a purge deletes move by ZERO (a
// deleted notice may have been read long ago, and the count claims nothing it
// cannot know), while the lead the purge wakes moves by its one new notice.
func TestAPurgeSaysWhoseInboxItEmptied(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	var got [][]inbox.Movement
	h.applier = worklet.NewApplier(func(m []inbox.Movement) {
		got = append(got, m)
	})
	create := taskRecord("t-1", worklet.OpCreate, newTask("t-1"), &worklet.Notify{
		Kind: worklet.ChangeCreated,
		Snapshot: worklet.Snapshot{
			Key: "ENG-1", Assignee: "bo", Watchers: []string{"cy"},
		},
	})
	if _, err := h.apply(create, time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	h.applier.Committed(context.Background())
	got = nil

	purge := taskRecord("t-1", worklet.OpPurge,
		map[string]any{"reason": "a duplicate import"}, &worklet.Notify{
			Kind: worklet.ChangePurged,
			Snapshot: worklet.Snapshot{
				Key: "ENG-1", Project: "ENG", ProjectLead: "lee",
			},
		})
	if _, err := h.apply(purge, time.Unix(1_700_000_200, 0).UTC()); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if left := h.value(`SELECT COUNT(*) FROM tracker_notifications
		WHERE recipient IN ('bo', 'cy')`); left != 0 {
		t.Fatalf("the purge left %d of the task's notices", left)
	}
	h.applier.Committed(context.Background())
	// TIMED FROM THE PURGE, not the create that wrote the notices it deleted.
	purged := time.Unix(1_700_000_200, 0).UTC()
	want := []inbox.Movement{
		moved("bo", 0, "t-1", worklet.ReasonAssignee, purged),
		moved("cy", 0, "t-1", worklet.ReasonWatcher, purged),
		moved("lee", 1, "t-1", worklet.ReasonPurged, purged),
	}
	if len(got) != 1 || !slices.Equal(got[0], want) {
		t.Fatalf("the purge announced %v, want %v — every inbox it emptied "+
			"by zero and the lead it woke by one", got, want)
	}

	// A REDELIVERY OF THE PURGE, let through by its own id, has nothing left
	// to delete, so it moves nobody.
	got = nil
	if _, err := h.applyAt(purge, time.Unix(1_700_000_200, 0).UTC(), h.seq); err != nil {
		t.Fatalf("redeliver the purge: %v", err)
	}
	h.applier.Committed(context.Background())
	if len(got) != 0 {
		t.Errorf("a redelivered purge announced %v", got)
	}
}
