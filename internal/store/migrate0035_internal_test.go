package store

import (
	"path"
	"path/filepath"
	"slices"
	"testing"
)

// migration0035 is the file under test, named once.
const migration0035 = "0035_a_revision_says_what_wrote_it.sql"

// 0035 CLASSIFIES THE REVISIONS AN UPGRADE ALREADY HOLDS BY THE LITERALS THEIR
// WRITERS USED, AND CLEARS THE ONE THAT WAS NEVER AN AUTHOR.
//
// A fresh file has no revisions, so a backfill that names a column wrongly or
// classifies one writer as another passes there. A node that upgrades holds
// its seed (`node`), the reconcile loop's writes (`reconcile loop`), an
// operator's writes under a token or a login, and adoptions recorded as
// `peer` — and each must come out as what actually wrote it, the adoption as
// NOT RECORDED rather than as a name nobody has.
func TestNode0035ClassifiesTheRevisionsAnUpgradeHolds(t *testing.T) {
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
	if !slices.Contains(files, migration0035) {
		t.Fatalf("%s is not among the node migrations %v", migration0035, files)
	}
	for _, name := range files {
		if name >= migration0035 {
			break
		}
		body, err := schemaFS.ReadFile(path.Join("schema", string(EstateNode), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.applyOne(ctx, name, string(body)); err != nil {
			t.Fatalf("bring the file to 0034: %v", err)
		}
	}

	rows := []struct{ id, by, source, wantBy, wantKind string }{
		{"seed", "node", "file", "node", "node"},
		{"loop", "reconcile loop", "api", "reconcile loop", "node"},
		{"token", "maya", "api", "maya", "operator"},
		{"login", "ops", "file", "ops", "operator"},
		{"rekey", "ops", "rekey", "ops", "operator"},
		{"adopted", "peer", "fleet", "", ""},
	}
	for _, row := range rows {
		if _, err := pool.ExecContext(ctx, `
			INSERT INTO company_config (revision_id, created_at, created_by, source,
				summary, payload)
			VALUES (?, 1, ?, ?, '', '{}')`, row.id, row.by, row.source); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}

	applied, err := db.migrate(ctx)
	if err != nil {
		t.Fatalf("migrate a populated 0034 database: %v", err)
	}
	if !slices.Contains(applied, migration0035) {
		t.Fatalf("applied %v, want %s among them", applied, migration0035)
	}
	for _, row := range rows {
		var by, kind string
		if err := pool.QueryRowContext(ctx,
			`SELECT created_by, created_by_kind FROM company_config WHERE revision_id = ?`,
			row.id).Scan(&by, &kind); err != nil {
			t.Fatal(err)
		}
		if by != row.wantBy || kind != row.wantKind {
			t.Errorf("%s: (created_by, kind) = (%q, %q) after the backfill, want (%q, %q)",
				row.id, by, kind, row.wantBy, row.wantKind)
		}
	}
}
