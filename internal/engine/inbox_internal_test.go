package engine

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE TRACKER APPLIER A RUNNING NODE BUILDS TELLS THE API WHOSE INBOX MOVED.
//
// The tracker says which inboxes a committed batch moved; the API pushes that
// to the sockets watching each seat. Between them is exactly this package — the
// register that builds the applier and the setter the API registers on — and a
// nil handed to the applier there compiles, runs and passes every tracker case,
// while every person's screen goes back to learning about their work a poll
// interval late. So the applier here is the REGISTER's, and the record is
// applied and committed the way the framework's loop does it.
func TestTheTrackerApplierTheRegisterBuildsFeedsTheInbox(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	e := &Engine{}
	var heard [][]tracker.InboxMovement
	e.SetOnInboxMoved(func(moved []tracker.InboxMovement) {
		heard = append(heard, moved)
	})
	entry, ok := registrationFor(tracker.Domain{}.Name())
	if !ok {
		t.Fatal("the register has no tracker domain")
	}
	applier, err := entry.NewApplier(&stateLog{nodeID: "node-a", applyHooks: e.applyHooks()})
	if err != nil {
		t.Fatalf("build the applier: %v", err)
	}

	at := time.Unix(1_700_000_000, 0).UTC()
	task := tracker.Task{
		V: tracker.DocumentVersion, ID: "t-1", Key: "ENG-1", Project: "ENG",
		Type: "task", Title: "a task", Status: tracker.StatusTodo,
		StatusGroup: tracker.GroupNotStarted, Priority: tracker.PriorityNormal,
		Rank: tracker.RankOrigin, CreatedAt: at, UpdatedAt: at,
	}
	title := "assigned"
	for seq, rec := range []tracker.MutationRecord{
		inboxRecord(t, tracker.OpCreate, task, nil),
		inboxRecord(t, tracker.OpPatch, tracker.TaskPatch{Title: &title}, &tracker.Notify{
			Kind:     tracker.ChangeStatus,
			Snapshot: tracker.Snapshot{Key: "ENG-1", Assignee: "bo"},
		}),
	} {
		body, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		record := statelog.Record{
			Envelope: statelog.Envelope{
				V: rec.V, Kind: string(rec.Subject.Kind),
				Subject: statelog.Subject{Kind: string(rec.Subject.Kind), ID: rec.Subject.ID},
				Op:      string(rec.Op), OpID: rec.OpID, Writer: rec.Writer,
			},
			Position: statelog.Position{Stream: tracker.Domain{}.Stream().Name, Seq: uint64(seq + 1)},
			Payload:  body,
			StoredAt: at.Add(time.Duration(seq+1) * time.Second),
		}
		if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := applier.Apply(t.Context(), tx, record, statelog.ApplyOptions{
				Now: record.StoredAt, StoredAt: record.StoredAt,
				MaxVariables: db.Caps().MaxVariables,
			})
			return err
		}); err != nil {
			t.Fatalf("apply %s: %v", rec.Op, err)
		}
		applier.Committed(t.Context())
	}

	want := []tracker.InboxMovement{
		{Handle: "bo", UnreadDelta: 1, Subject: "t-1", Reason: tracker.ReasonAssignee},
	}
	if len(heard) != 1 || !slices.Equal(heard[0], want) {
		t.Fatalf("the API heard %v, want exactly one batch %v", heard, want)
	}
}

// inboxRecord is one tracker record about task t-1.
func inboxRecord(t *testing.T, op tracker.OpKind, payload any,
	notify *tracker.Notify) tracker.MutationRecord {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "t-1-" + string(op),
			Subject: tracker.TaskSubject("t-1"), Op: op,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Writer:    "node-a", Scope: tracker.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: body, Actor: "ana", ActorKind: tracker.AuthorHuman,
		Notify: notify,
	}
}
