package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
)

// AN EVICTION ROW WRITTEN WHILE ROWS CARRIED A GATE KIND COMES THROUGH THE
// DROP OF THAT KIND WHOLE.
//
// Replicated migration 0034 gave both eviction tables a `kind` (`eviction` or
// `release`), and 0037 drops it because nothing publishes a release any more.
// Nothing re-applies the eviction records behind these rows, so a row lost
// here is an evicted node every applier starts applying again and the trim
// starts counting again. So the case stands a database up at 0034, writes one
// row into each table in that shape, and lets the real migrator take it to the
// current schema: the rows must survive field for field, the column must be
// gone, and a row must still be writable without naming it.
func TestAnEvictionRowFromTheKindEraComesThroughTheDropOfItsKind(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "replicated.db")
	old := migratedThrough(t, EstatePartition, path,
		"0034_a_gate_says_whether_it_was_a_release.sql")
	for _, stmt := range []string{
		`INSERT INTO tracker_evictions
			(node_id, log_stream, from_position, at, by, readmitted_position,
			 readmitted_at, kind)
		VALUES ('node-gone', 'CREWLET_TRACKER_LOG', 41, 1700000000, 'ops-1', 57,
			1700000060, 'eviction')`,
		`INSERT INTO pages_evictions
			(node_id, at, by, from_position, readmitted_position, version, kind)
		VALUES ('node-gone', 1700000000, 'ops-1', 43, NULL, 43, 'eviction')`,
	} {
		if _, err := old.ExecContext(t.Context(), stmt); err != nil {
			_ = old.Close()
			t.Fatalf("write a row in 0034's shape: %v", err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := OpenEstate(t.Context(), EstatePartition, path, Options{})
	if err != nil {
		t.Fatalf("migrate the kind-era database forward: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	applied, err := db.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(applied, "0037_an_eviction_row_names_no_gate_kind.sql") {
		t.Fatalf("0037 did not run on a database at 0034: applied %v", applied)
	}

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, table := range []string{"tracker_evictions", "pages_evictions"} {
			if columns := columnsOf(t, tx, table); slices.Contains(columns, "kind") {
				t.Errorf("%s still has a kind column: %v", table, columns)
			}
		}
		type trackerRow struct {
			stream, by             string
			from, at, back, backAt int64
		}
		var tracker trackerRow
		if err := tx.QueryRowContext(t.Context(), `
			SELECT log_stream, by, from_position, at, readmitted_position, readmitted_at
			FROM tracker_evictions WHERE node_id = 'node-gone'`).Scan(&tracker.stream,
			&tracker.by, &tracker.from, &tracker.at, &tracker.back, &tracker.backAt); err != nil {
			t.Errorf("the tracker's eviction row did not come through: %v", err)
		} else if want := (trackerRow{"CREWLET_TRACKER_LOG", "ops-1", 41, 1700000000, 57,
			1700000060}); tracker != want {
			t.Errorf("the tracker's eviction row came through as %+v, want %+v", tracker, want)
		}
		var (
			by                string
			from, at, version int64
			readmitted        sql.NullInt64
		)
		if err := tx.QueryRowContext(t.Context(), `
			SELECT by, from_position, at, readmitted_position, version
			FROM pages_evictions WHERE node_id = 'node-gone'`).Scan(&by, &from, &at,
			&readmitted, &version); err != nil {
			t.Errorf("the knowledge base's eviction row did not come through: %v", err)
		} else if by != "ops-1" || from != 43 || at != 1700000000 || readmitted.Valid ||
			version != 43 {
			t.Errorf("the knowledge base's eviction row came through as by=%s from=%d "+
				"at=%d readmitted=%v version=%d", by, from, at, readmitted, version)
		}
		// AND AN APPLIER THAT NAMES NO KIND STILL WRITES BOTH, which is
		// the shape every build after 0037 writes.
		if _, err := tx.ExecContext(t.Context(), `
			INSERT INTO tracker_evictions
				(node_id, log_stream, from_position, at, by, readmitted_position, readmitted_at)
			VALUES ('node-next', 'CREWLET_TRACKER_LOG', 90, 1700000100, 'ops-2', NULL, NULL)`); err != nil {
			t.Errorf("write a tracker eviction naming no kind: %v", err)
		}
		if _, err := tx.ExecContext(t.Context(), `
			INSERT INTO pages_evictions
				(node_id, at, by, from_position, readmitted_position, version)
			VALUES ('node-next', 1700000100, 'ops-2', 91, NULL, 91)`); err != nil {
			t.Errorf("write a knowledge-base eviction naming no kind: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the migrated rows: %v", err)
	}
}

// columnsOf is a table's column names, in declaration order.
func columnsOf(t *testing.T, tx *sql.Tx, table string) []string {
	t.Helper()
	rows, err := tx.QueryContext(t.Context(),
		`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("read %s's columns: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("read %s's columns: %v", table, err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s's columns: %v", table, err)
	}
	return out
}
