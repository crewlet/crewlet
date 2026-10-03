package store

import (
	"path"
	"path/filepath"
	"slices"
	"testing"
)

// migration0041 is the file under test, named once.
const migration0041 = "0041_a_write_names_its_author_beside_its_credential.sql"

// 0041 RUNS AFTER 0035 AND ADDS ONLY WHAT 0035 DID NOT.
//
// Both files name `company_config.created_by_kind`: 0035 adds it, and 0041 was
// written on a line that had not seen 0035 and added it too. A second ADD
// COLUMN of an existing column fails the migration, and a failed migration is
// an Open that never returns — so the property is that 0041, run on a file
// 0035 has already shaped, applies, and leaves every column the write path
// names in place: the credential beside a revision's author, and the kind and
// the credential beside a stored secret's.
//
// A fresh file proves it only incidentally, so this brings a file to the
// migration before 0041 — 0035 included — holding a revision 0035 classified,
// and runs the rest from there.
//
// Mutation: restore 0041's `ADD COLUMN created_by_kind`, and the migrate fails.
func TestNode0041AppliesOnTopOf0035(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, err := openPrepared(ctx, filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("openPrepared: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	db := &DB{sql: pool, estate: EstateNode}

	if _, err := pool.ExecContext(ctx, `
		CREATE TABLE schema_migrations (
			version    TEXT    NOT NULL PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	files, err := schemaVersions(EstateNode)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(files, migration0041) {
		t.Fatalf("%s is not among the node migrations %v", migration0041, files)
	}
	for _, name := range files {
		if name >= migration0041 {
			break
		}
		body, err := schemaFS.ReadFile(path.Join("schema", string(EstateNode), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.applyOne(ctx, name, string(body)); err != nil {
			t.Fatalf("bring the file to the migration before 0041: %v", err)
		}
	}
	if !slices.Contains(columnNames(t, db, "company_config"), "created_by_kind") {
		t.Fatal("company_config has no created_by_kind before 0041 — 0035 did " +
			"not run, so this case is not testing the order it names")
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO company_config (revision_id, created_at, created_by,
			created_by_kind, source, summary, payload)
		VALUES ('seed', 1, 'node-a', 'system', 'file', '', '{}')`); err != nil {
		t.Fatalf("seed a revision 0035's column describes: %v", err)
	}

	applied, err := db.migrate(ctx)
	if err != nil {
		t.Fatalf("migrate a file 0035 has shaped: %v", err)
	}
	if !slices.Contains(applied, migration0041) {
		t.Fatalf("applied %v, want %s among them", applied, migration0041)
	}
	for table, want := range map[string][]string{
		"company_config": {"created_by_kind", "operator_id"},
		"secret_values":  {"updated_by_kind", "operator_id"},
	} {
		got := columnNames(t, db, table)
		for _, column := range want {
			if !slices.Contains(got, column) {
				t.Errorf("%s has no %s after 0041: %v", table, column, got)
			}
		}
	}
	var kind, operator string
	if err := pool.QueryRowContext(ctx, `SELECT created_by_kind, operator_id
		FROM company_config WHERE revision_id = 'seed'`).Scan(&kind, &operator); err != nil {
		t.Fatal(err)
	}
	if kind != "system" || operator != "" {
		t.Errorf("the seeded revision reads (kind %q, operator %q) after 0041, "+
			"want (system, none) — 0041 adds columns and rewrites no row", kind, operator)
	}
}

// columnNames is one node-estate table's column names, in declaration order.
func columnNames(t *testing.T, db *DB, table string) []string {
	t.Helper()
	rows, err := db.sql.QueryContext(t.Context(), `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("read %s's columns: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("%s has no columns, so it does not exist and every assertion "+
			"about it would pass vacuously", table)
	}
	return out
}
