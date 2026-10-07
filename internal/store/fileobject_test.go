package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
)

// A FILE ROW WRITTEN WHEN FILES WERE CHUNKS COMES THROUGH, NAMING NO OBJECT.
//
// Replicated migration 0038 drops `tracker_file_chunks` and the row's chunk
// count, and gives every row an `object` column. Nothing re-applies the file
// records behind the rows, so the migration has to leave exactly what a node
// replaying those records from nothing writes — the row kept, every column it
// had but the count, and NULL for its object. A row lost here is a
// file the company can no longer even list; a row given anything but NULL is
// one a migrated node and a replaying node disagree about for good. So the
// case stands a database up at 0037, writes files and their chunk rows in
// that shape, and lets the real migrator take it forward.
func TestAFileRowFromTheChunkEraComesThroughNamingNoObject(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "replicated.db")
	old := migratedThrough(t, EstateReplicated, path, "0037_an_eviction_row_names_no_gate_kind.sql")
	type file struct {
		id, project, path, hash string
		size                    int64
		removed                 bool
	}
	files := []file{
		{"ENG.a", "ENG", "reports/q3.md", string(objstore.HashOf([]byte("q3"))), 3_000_000, false},
		{"ENG.b", "ENG", "notes.txt", string(objstore.HashOf([]byte("notes"))), 5, false},
		{"OPS.c", "OPS", "gone.bin", "", 0, true},
	}
	for _, f := range files {
		var removedAt any
		chunks := (f.size + objstore.MiB - 1) / objstore.MiB
		if f.removed {
			removedAt = int64(1_700_000_100)
		}
		if _, err := old.ExecContext(t.Context(), `
			INSERT INTO tracker_files
				(id, project_key, path, content_type, hash, size, chunks,
				 created_by, created_at, updated_by, updated_at,
				 removed_by, removed_at, version, document)
			VALUES (?,?,?,'text/plain',?,?,?,'dev',1700000000,'dev',1700000000,?,?,7,?)`,
			f.id, f.project, f.path, f.hash, f.size, chunks,
			map[bool]string{true: "ops"}[f.removed], removedAt,
			[]byte(`{"v":1,"chunks":[]}`)); err != nil {
			_ = old.Close()
			t.Fatalf("write a file row in 0037's shape: %v", err)
		}
		for seq := range chunks {
			if _, err := old.ExecContext(t.Context(), `
				INSERT INTO tracker_file_chunks (file_id, seq, chunk, size)
				VALUES (?, ?, ?, ?)`,
				f.id, seq, string(objstore.HashOf(fmt.Appendf(nil, "%s/%d", f.id, seq))),
				objstore.MiB); err != nil {
				_ = old.Close()
				t.Fatalf("write a chunk row in 0037's shape: %v", err)
			}
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := OpenEstate(t.Context(), EstateReplicated, path, Options{})
	if err != nil {
		t.Fatalf("migrate the chunk-era database forward: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	applied, err := db.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(applied, "0038_a_file_row_names_one_object.sql") {
		t.Fatalf("0038 did not run on a database at 0037: applied %v", applied)
	}

	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		for _, f := range files {
			var project, path, hash, removedBy string
			var size int64
			var version int
			var object sql.NullString
			var removedAt sql.NullInt64
			if err := tx.QueryRowContext(t.Context(), `
				SELECT project_key, path, hash, size, version, object, removed_by, removed_at
				FROM tracker_files WHERE id = ?`, f.id).Scan(&project, &path, &hash,
				&size, &version, &object, &removedBy, &removedAt); err != nil {
				return fmt.Errorf("read %s back: %w", f.id, err)
			}
			switch {
			case project != f.project || path != f.path || hash != f.hash || size != f.size || version != 7:
				t.Errorf("%s came through as %s/%s %s %d bytes at %d", f.id, project, path, hash, size, version)
			case object.Valid:
				t.Errorf("%s came through naming object %q; a row from before objects names none",
					f.id, object.String)
			case removedAt.Valid != f.removed:
				t.Errorf("%s came through removed=%v", f.id, removedAt.Valid)
			}
		}
		var columns []string
		rows, err := tx.QueryContext(t.Context(), `SELECT name FROM pragma_table_info('tracker_files')`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			columns = append(columns, name)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if slices.Contains(columns, "chunks") || !slices.Contains(columns, "object") {
			t.Errorf("tracker_files has columns %v, want object and no chunks", columns)
		}
		for name, want := range map[string]bool{
			"tracker_files_object_idx":      true,
			"tracker_file_chunks":           false,
			"tracker_file_chunks_chunk_idx": false,
		} {
			var n int
			if err := tx.QueryRowContext(t.Context(),
				`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n); err != nil {
				return err
			}
			if (n == 1) != want {
				t.Errorf("%q is in the schema %d times, want present=%v", name, n, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read the migrated estate: %v", err)
	}
}
