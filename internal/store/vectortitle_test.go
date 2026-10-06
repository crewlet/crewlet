package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
)

// A DATABASE FROM BEFORE 0038 COMES THROUGH IT WHOLE: its vectors gain a title
// that reads as unknown, and its tasks lose two columns nothing ever wrote.
//
// Replicated migration 0038 adds `kb_vectors.title` — what a page's vector was
// computed from, so a rename selects it again — and drops `tracker_tasks`'
// `embed_rev` and `search_rev` with the partial index on the first, which no
// build ever wrote anything but a zero into. So the case stands a database up
// at 0037 with one task and one vector in that shape, lets the real migrator
// take it forward, and asserts both rows survive: the vector with an EMPTY
// title, which the duty reads as a title that moved and settles on its next
// tick, and the task without the two columns — and that the shape every build
// after 0038 writes is writable.
func TestADatabaseFromBefore0038ComesThroughItWhole(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "replicated.db")
	old := migratedThrough(t, EstateReplicated, path,
		"0037_an_eviction_row_names_no_gate_kind.sql")
	for _, stmt := range []string{
		`INSERT INTO tracker_tasks
			(id, key, project_key, root_id, type, title, status, status_group,
			 rank, version, created_at, updated_at, search_rev, embed_rev, document)
		VALUES ('t-1', 'ENG-1', 'ENG', 't-1', 'task', 'Rate limits', 'todo',
			'not_started', 'a0', 7, 1, 2, 0, 0, x'7b7d')`,
		`INSERT INTO kb_vectors
			(source, source_id, container, search_shard, model, dim, source_rev,
			 text_sha, embedding, embedded_at, version)
		VALUES ('page', 'p-1', 'eng', 3, 'm', 8, 4, 'sha', x'00', 1, 9)`,
	} {
		if _, err := old.ExecContext(t.Context(), stmt); err != nil {
			_ = old.Close()
			t.Fatalf("write a row in 0037's shape: %v", err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := OpenEstate(t.Context(), EstateReplicated, path, Options{})
	if err != nil {
		t.Fatalf("migrate the database forward: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	applied, err := db.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(applied, "0038_a_vector_names_its_title_and_the_embed_columns_go.sql") {
		t.Fatalf("0038 did not run on a database at 0037: applied %v", applied)
	}

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		columns := columnsOf(t, tx, "tracker_tasks")
		for _, gone := range []string{"embed_rev", "search_rev"} {
			if slices.Contains(columns, gone) {
				t.Errorf("tracker_tasks still has %s: %v", gone, columns)
			}
		}
		var indexes int
		if err := tx.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM sqlite_master
			WHERE type = 'index' AND name = 'tracker_tasks_embed_idx'`).Scan(&indexes); err != nil {
			return err
		}
		if indexes != 0 {
			t.Error("tracker_tasks_embed_idx survived the drop of the column it indexed")
		}
		var title string
		var version int64
		if err := tx.QueryRowContext(t.Context(),
			`SELECT title, version FROM tracker_tasks WHERE id = 't-1'`).Scan(&title, &version); err != nil {
			t.Errorf("the task did not come through: %v", err)
		} else if title != "Rate limits" || version != 7 {
			t.Errorf("the task came through as %q at version %d", title, version)
		}
		var vectorTitle, container string
		var rev int64
		if err := tx.QueryRowContext(t.Context(), `
			SELECT title, container, source_rev FROM kb_vectors
			WHERE source = 'page' AND source_id = 'p-1'`).Scan(&vectorTitle, &container, &rev); err != nil {
			t.Errorf("the vector did not come through: %v", err)
		} else if vectorTitle != "" || container != "eng" || rev != 4 {
			t.Errorf("the vector came through titled %q in %q at %d, want an empty "+
				"title in eng at 4", vectorTitle, container, rev)
		}
		// AND THE SHAPES EVERY BUILD AFTER 0038 WRITES.
		if _, err := tx.ExecContext(t.Context(), `
			INSERT INTO tracker_tasks
				(id, key, project_key, root_id, type, title, status, status_group,
				 rank, version, created_at, updated_at, document)
			VALUES ('t-2', 'ENG-2', 'ENG', 't-2', 'task', 'Next', 'todo',
				'not_started', 'a1', 8, 1, 2, x'7b7d')`); err != nil {
			t.Errorf("write a task naming neither dropped column: %v", err)
		}
		if _, err := tx.ExecContext(t.Context(), `
			UPDATE kb_vectors SET title = 'Runbook'
			WHERE source = 'page' AND source_id = 'p-1'`); err != nil {
			t.Errorf("write a vector's title: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the migrated rows: %v", err)
	}
}
