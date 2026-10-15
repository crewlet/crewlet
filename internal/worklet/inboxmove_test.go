package worklet_test

import (
	"context"
	"database/sql"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/worklet"
)

// A COMMITTED BATCH SAYS WHOSE INBOX MOVED — once per person, only after the
// commit, and never for a notice it did not write.
//
// This is how a Crewlet-only person learns they have work: the applier on
// every node announces to that node's own sockets, so nothing about it can be
// a courtesy the writer's goroutine performs. Three properties carry it:
//
//   - AFTER THE COMMIT. A movement announced from inside the transaction would
//     announce a notice no row holds if the body failed and re-ran.
//   - ONLY WHAT WAS WRITTEN. A redelivery writes no inbox row, so it moved
//     nobody's inbox; announcing it anyway would refresh every watcher's
//     screen for nothing on every redelivered record.
//   - ONE PER HANDLE PER BATCH, carrying the newest subject: an import that
//     files a hundred tasks for one person is one push to their screen, not a
//     hundred.
func TestACommittedBatchSaysWhoseInboxMoved(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	var got [][]worklet.InboxMovement
	h.applier = worklet.NewApplier("node-a", func(m []worklet.InboxMovement) {
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
	// NOTHING BEFORE THE COMMIT'S OWN HALF, which is what the framework
	// calls after the transaction lands.
	if len(got) != 0 {
		t.Fatalf("a movement was announced before the commit: %v", got)
	}
	h.applier.Committed(context.Background())
	want := []worklet.InboxMovement{
		{Handle: "bo", UnreadDelta: 1, Subject: "t-1", Reason: worklet.ReasonAssignee},
		{Handle: "cy", UnreadDelta: 1, Subject: "t-1", Reason: worklet.ReasonWatcher},
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
	// AND ONE THAT GETS PAST THE HISTORY ROW — a history row reanchored
	// away while the notices it wrote remain, which is the state the inbox
	// reader already names — still writes no notice, and still moves
	// nobody: what is announced is what the inbox rows gained, never what a
	// record could have said.
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
	// naming the newer.
	second := taskRecord("t-2", worklet.OpCreate, newTask("t-2"), &worklet.Notify{
		Kind: worklet.ChangeCreated, Snapshot: worklet.Snapshot{Key: "ENG-2", Assignee: "bo"},
	})
	second.Subject = worklet.TaskSubject("t-2")
	third := taskRecord("t-3", worklet.OpCreate, newTask("t-3"), &worklet.Notify{
		Kind: worklet.ChangeCreated, Snapshot: worklet.Snapshot{Key: "ENG-3", Assignee: "bo"},
	})
	third.Subject = worklet.TaskSubject("t-3")
	for _, rec := range []worklet.MutationRecord{second, third} {
		if _, err := h.apply(rec, time.Unix(1_700_000_300, 0).UTC()); err != nil {
			t.Fatalf("create %s: %v", rec.Subject.ID, err)
		}
	}
	h.applier.Committed(context.Background())
	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("one batch for one person announced %v, want one movement", got)
	}
	if m := got[0][0]; m.Handle != "bo" || m.UnreadDelta != 2 || m.Subject != "t-3" {
		t.Errorf("the batch announced %+v, want bo moved by 2 with t-3 newest", m)
	}
}

// A PURGE SAYS WHOSE INBOX IT EMPTIED, as surely as a commit says whose it
// filled.
//
// A purge deletes the notices its task's records routed — they hold excerpts,
// and destroying content is what a purge is for — on every data node at once.
// An applier that announced only what it ADDED told nobody: a person whose
// inbox listed the task went on seeing notices no node held until their
// screen's next poll, and opening one answered that the task does not exist.
// So the recipients of what a purge deletes move too, by ZERO — a notice
// deleted may have been read long ago, and the count claims nothing it cannot
// know — while the lead the purge itself wakes moves by its one new notice.
func TestAPurgeSaysWhoseInboxItEmptied(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	var got [][]worklet.InboxMovement
	h.applier = worklet.NewApplier("node-a", func(m []worklet.InboxMovement) {
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
	want := []worklet.InboxMovement{
		{Handle: "bo", UnreadDelta: 0, Subject: "t-1", Reason: worklet.ReasonAssignee},
		{Handle: "cy", UnreadDelta: 0, Subject: "t-1", Reason: worklet.ReasonWatcher},
		{Handle: "lee", UnreadDelta: 1, Subject: "t-1", Reason: worklet.ReasonPurged},
	}
	if len(got) != 1 || !slices.Equal(got[0], want) {
		t.Fatalf("the purge announced %v, want %v — every inbox it emptied "+
			"by zero and the lead it woke by one", got, want)
	}

	// A REDELIVERY OF THE PURGE deletes nothing — the marker lets it
	// through by its own id, and there is nothing left to delete — so it
	// moves nobody.
	got = nil
	if _, err := h.applyAt(purge, time.Unix(1_700_000_200, 0).UTC(), h.seq); err != nil {
		t.Fatalf("redeliver the purge: %v", err)
	}
	h.applier.Committed(context.Background())
	if len(got) != 0 {
		t.Errorf("a redelivered purge announced %v", got)
	}
}

// AN INBOX MOVEMENT CARRIES NO CONTENT.
//
// It rides a socket frame to whoever watches that seat, and what it is for is
// telling a screen to re-read the inbox through the SAME question, and the same
// authority, as the poll it replaces. A field carrying an excerpt, a title or
// an author would be a read path around that authority — so the set of fields
// is held here, and adding one is a decision somebody has to make in this
// file.
func TestAnInboxMovementCarriesNoContent(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"Handle": "handle", "UnreadDelta": "unread_delta",
		"Subject": "subject", "Reason": "reason",
	}
	typ := reflect.TypeFor[worklet.InboxMovement]()
	if typ.NumField() != len(want) {
		t.Fatalf("an inbox movement has %d fields, want exactly %d identifiers",
			typ.NumField(), len(want))
	}
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag, ok := want[field.Name]
		if !ok {
			t.Errorf("an inbox movement carries %s, which is not one of the four "+
				"identifiers it may name", field.Name)
			continue
		}
		if got := field.Tag.Get("json"); got != tag {
			t.Errorf("%s is %q on the wire, want %q", field.Name, got, tag)
		}
	}
}
