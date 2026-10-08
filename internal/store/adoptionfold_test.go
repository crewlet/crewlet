package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
)

// AN ADOPTION ROW CARRIES NO FOLD FLAG, and one is written without naming it.
//
// Node migration 0030 gave `statelog_adoption` a `ledger_folded` column for a
// boot-time fold this tree no longer has; 0040 drops it, because nothing
// reads it and the only value this build ever wrote was the constant 1. The case runs the real migrator over a fresh node estate, which
// is what proves the driver takes the DROP COLUMN, and writes the row the
// adopter writes.
func TestAnAdoptionRowCarriesNoFoldFlag(t *testing.T) {
	t.Parallel()
	db, err := OpenEstate(t.Context(), EstateNode,
		filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open a node estate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	applied, err := db.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(applied, "0040_an_adoption_row_forgets_the_fold.sql") {
		t.Fatalf("0040 did not run on a fresh node estate: applied %v", applied)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		if columns := columnsOf(t, tx, "statelog_adoption"); slices.Contains(columns, "ledger_folded") {
			t.Errorf("statelog_adoption still has a ledger_folded column: %v", columns)
		}
		if _, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_adoption (started_at, donor, manifest, completed_at)
			VALUES (1700000000, 'node-a', 'sha', NULL)`); err != nil {
			t.Errorf("write an adoption row naming no fold flag: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// columnsOf names a table's columns as the driver reports them.
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
