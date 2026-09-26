package tracker

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// A ROW'S COST IS ITS OWN, AND ONLY THE ROWS ASKED ABOUT ARE ANSWERED.
//
// `spendOf` decorates rows somebody already read, so the two ways it can be
// wrong are both silent on a board: a cost filed under the wrong id is a card
// showing its neighbour's tokens, and an id nobody asked about in the answer
// is a map a caller ranges over and draws a card for a task that is not on the
// board. An id that does not exist is simply absent — never a zero entry,
// which would read as "this task cost nothing" about a task that is not there.
func TestSpendOfAnswersExactlyTheAskedIds(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	w, err := db.Replicated().Writer(t.Context())
	if err != nil {
		t.Fatalf("take the writer: %v", err)
	}
	defer w.Close()

	seeded := map[string]RowSpend{
		"t-a": {Tokens: 1400, Turns: 2, Workers: 3, SentBack: 1, Reopens: 1},
		"t-b": {Tokens: 90, Turns: 1},
		"t-c": {Tokens: 7000, Turns: 5, Workers: 1, SentBack: 2, Reopens: 4},
	}
	var got map[string]RowSpend
	if err := w.Tx(t.Context(), func(tx *sql.Tx) error {
		for id, s := range seeded {
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO tracker_tasks
					(id, key, project_key, filed_unit, routing_unit, root_id,
					 type, title, status, status_group, rank, spend_tokens,
					 spend_turns, spend_workers, spend_sent_back, reopens,
					 created_at, updated_at, version, document)
				VALUES (?,?,'ENG','eng','eng',?,'task','a task','todo',
				        'not_started','a0',?,?,?,?,?,0,0,1,'{}')`,
				id, "ENG-"+id, id, s.Tokens, s.Turns, s.Workers, s.SentBack,
				s.Reopens); err != nil {
				return err
			}
		}
		got, err = spendOf(t.Context(), tx, []string{"t-a", "t-c", "t-gone"})
		return err
	}); err != nil {
		t.Fatalf("seed and read: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("asked about t-a, t-c and a task that does not exist and got "+
			"%d answers (%v), want exactly the two that exist", len(got), got)
	}
	for _, id := range []string{"t-a", "t-c"} {
		if got[id] != seeded[id] {
			t.Errorf("%s cost %+v, want its own %+v — a cost filed under the "+
				"wrong id is a card showing its neighbour's tokens", id, got[id],
				seeded[id])
		}
	}
	if _, held := got["t-b"]; held {
		t.Error("t-b was answered although nobody asked about it")
	}
	if _, held := got["t-gone"]; held {
		t.Error("a task that does not exist was answered — a zero entry reads " +
			"as a task that cost nothing")
	}
}
