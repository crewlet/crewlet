package tracker_test

import (
	"context"
	"database/sql"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tracker"
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
	var got [][]tracker.InboxMovement
	h.applier = tracker.NewApplier("node-a", func(m []tracker.InboxMovement) {
		got = append(got, m)
	})

	// A QUIET CREATE concerns nobody.
	if _, err := h.apply(taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	h.applier.Committed(context.Background())
	if len(got) != 0 {
		t.Fatalf("a quiet commit moved %v", got)
	}

	loud := taskRecord("t-1", tracker.OpPatch, tracker.TaskPatch{Title: ptr("louder")},
		&tracker.Notify{
			Kind: tracker.ChangeStatus,
			Snapshot: tracker.Snapshot{
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
	want := []tracker.InboxMovement{
		{Handle: "bo", UnreadDelta: 1, Subject: "t-1", Reason: tracker.ReasonAssignee},
		{Handle: "cy", UnreadDelta: 1, Subject: "t-1", Reason: tracker.ReasonWatcher},
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
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
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
	second := taskRecord("t-2", tracker.OpCreate, newTask("t-2"), &tracker.Notify{
		Kind: tracker.ChangeCreated, Snapshot: tracker.Snapshot{Key: "ENG-2", Assignee: "bo"},
	})
	second.Subject = tracker.TaskSubject("t-2")
	third := taskRecord("t-3", tracker.OpCreate, newTask("t-3"), &tracker.Notify{
		Kind: tracker.ChangeCreated, Snapshot: tracker.Snapshot{Key: "ENG-3", Assignee: "bo"},
	})
	third.Subject = tracker.TaskSubject("t-3")
	for _, rec := range []tracker.MutationRecord{second, third} {
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
	typ := reflect.TypeFor[tracker.InboxMovement]()
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
