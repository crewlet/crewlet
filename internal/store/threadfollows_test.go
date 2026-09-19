package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// THE HANDOFF'S ONLY PATH AGAINST A POPULATED TABLE.
//
// internal/notify/followsync runs against a fake in its own suite, which proves
// its rules and nothing about SQL. This is the other half: the column list, the
// timestamp decode and the four-segment WHERE are exercised nowhere else, and
// they are what every row written before node migration 0028 travels through
// exactly once.
func TestTheFollowsHandoffSourceReadsAndDrainsRealRows(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Written the way the build before the move wrote them, so this test is
	// about the rows that actually exist rather than about its own INSERT.
	created := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 9, 1, 14, 30, 0, 0, time.UTC)
	for _, row := range [][]any{
		{"slack", "agent-swe", "C1", "t-1", "mention", created, updated},
		{"slack", "agent-swe", "C1", "t-2", "explicit", created, updated.Add(time.Hour)},
		{"mattermost", "agent-ops", "", "root-9", "collective", created, updated},
	} {
		if _, err := db.SQL().ExecContext(t.Context(),
			`INSERT INTO chat_thread_follows
			 (backend, agent_handle, channel_id, thread_id, reason, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			row[0], row[1], row[2], row[3], row[4],
			store.EncodeTime(row[5].(time.Time)), store.EncodeTime(row[6].(time.Time))); err != nil {
			t.Fatalf("seed %v: %v", row[3], err)
		}
	}

	follows := db.ThreadFollows()
	rows, err := follows.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("listed %d row(s), want 3", len(rows))
	}
	// ORDERED, so a handoff interrupted part way resumes over the same
	// sequence rather than a driver's whim.
	if rows[0].Backend != "mattermost" || rows[1].Thread != "t-1" || rows[2].Thread != "t-2" {
		t.Errorf("order = %v/%v/%v, want it stable by identity",
			rows[0].Backend, rows[1].Thread, rows[2].Thread)
	}
	// AN EMPTY CHANNEL IS A REAL VALUE, not a missing one: Mattermost's
	// root-post ids carry the thread and a follow may have no channel.
	if rows[0].Channel != "" || rows[0].Reason != "collective" {
		t.Errorf("the mattermost row came back %+v", rows[0])
	}
	if !rows[1].UpdatedAt.Equal(updated) {
		t.Errorf("updated_at = %v, want %v — the stamp decodes through "+
			"store.DecodeTime and nothing else reads this column",
			rows[1].UpdatedAt, updated)
	}
	if rows[2].Reason != "explicit" {
		t.Errorf("reason = %q, want it carried: `explicit` is the one with no "+
			"mention coming to re-establish it", rows[2].Reason)
	}

	// DROPPED BY IDENTITY, and only the one named.
	gone, err := follows.Drop(t.Context(), rows[1])
	if err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if !gone {
		t.Error("Drop reported it removed nothing")
	}
	// AND A ROW ALREADY GONE ANSWERS FALSE RATHER THAN FAILING, which is
	// what lets a re-run of an interrupted handoff be ordinary.
	if gone, err := follows.Drop(t.Context(), rows[1]); err != nil {
		t.Errorf("dropping an absent row raised: %v", err)
	} else if gone {
		t.Error("Drop reported it removed a row that was already gone")
	}

	left, err := follows.List(t.Context())
	if err != nil {
		t.Fatalf("List after Drop: %v", err)
	}
	if len(left) != 2 {
		t.Fatalf("%d row(s) left, want 2 — Drop took more than the one it named", len(left))
	}
	for _, row := range left {
		if row.Thread == "t-1" {
			t.Error("the dropped row is still there")
		}
	}
}
