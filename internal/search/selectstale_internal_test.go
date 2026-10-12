package search

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A SELECTION READS EACH OPENING WITH THE PAIR ITS CORPUS NAMES.
//
// The plan gate explains the opening read a corpus's own function returns
// ([TaskOpeningRead]), statement and arguments. The selection used to be handed
// the bare statement and bind its arguments itself, so the pair the gate
// certified was a copy of what ran rather than what ran. Handed a read whose
// arguments are its own, the selection must answer with what that pair reads —
// and a read that names another statement for a later source is refused rather
// than run as the statement prepared for the first.
func TestASelectionReadsEachOpeningWithThePairItsCorpusNames(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	for i, id := range []string{"t-a", "t-b"} {
		document, err := json.Marshal(map[string]string{"body": "the body of " + id})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Replicated().Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO tracker_tasks
					(id, key, project_key, root_id, type, title, status,
					 status_group, rank, version, created_at, updated_at,
					 document)
				VALUES (?, ?, 'ENG', ?, 'task', ?, 'todo', 'open', 'm0',
				        1, 0, ?, ?)`,
				id, strings.ToUpper(id), id, id, i+1, document)
			return err
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	selection, args := TaskSelection("m", 8, 10)
	run := func(opening openingRead) ([]Document, error) {
		var stale []Document
		err := db.Replicated().Reader().Read(ctx, func(tx *sql.Tx) error {
			var err error
			stale, err = selectStale(ctx, tx, selection, args, 10, Held{}, opening)
			return err
		})
		return stale, err
	}

	stale, err := run(func(id string) (string, []any) {
		return `SELECT ? || t.id FROM tracker_tasks t WHERE t.id = ?`, []any{"read as ", id}
	})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(stale) != 2 {
		t.Fatalf("selected %d task(s), want both", len(stale))
	}
	for _, doc := range stale {
		if want := "read as " + doc.ID; doc.Body != want {
			t.Errorf("%s's opening read as %q, want %q — the selection ran a "+
				"statement or arguments other than the ones its corpus named",
				doc.ID, doc.Body, want)
		}
	}

	if _, err := run(func(id string) (string, []any) {
		if id == "t-a" {
			return `SELECT 'first' FROM tracker_tasks t WHERE t.id = ?`, []any{id}
		}
		return `SELECT 'another' FROM tracker_tasks t WHERE t.id = ?`, []any{id}
	}); err == nil {
		t.Fatal("an opening read naming another statement for its second source " +
			"was run as the statement prepared for the first")
	}
}
