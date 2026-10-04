package store

import (
	"path"
	"path/filepath"
	"slices"
	"testing"
)

// migration0038 is the file under test, named once.
const migration0038 = "0038_identity_and_access.sql"

// 0038 TAKES A POPULATED 0037 DATABASE TO THIS BUILD'S SHAPE, AND REWRITES ONLY
// THE ROWS IT NAMES.
//
// A fresh file proves the shape only — every table it touches is empty there —
// so this brings a file to 0037, populates it in 0037's shapes and runs the
// rest from there:
//
//   - a revision and a stored secret keep every value they held and read the
//     author columns 0038 adds as EMPTY, because nothing recorded those facts;
//   - an adoption row survives the drop of the column beside it;
//   - the related-agent index holds AGENT IDS ONLY: every name-keyed party is
//     gone, and each event that carries an agent id is filed under it.
//
// Mutation: take the DELETE out of 0038's party rewrite and the names survive;
// take the INSERT out and the agent's own event drops out of the filter; take
// the DROP COLUMN out and `ledger_folded` is reported.
func TestNode0038UpgradesAPopulated0037Database(t *testing.T) {
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
	if !slices.Contains(files, migration0038) {
		t.Fatalf("%s is not among the node migrations %v", migration0038, files)
	}
	for _, name := range files {
		if name >= migration0038 {
			break
		}
		body, err := schemaFS.ReadFile(path.Join("schema", string(EstateNode), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.applyOne(ctx, name, string(body)); err != nil {
			t.Fatalf("bring the file to 0037: %v", err)
		}
	}

	// 0037's SHAPES: parties keyed on names, an adoption carrying the fold
	// marker, and a revision and a secret with one author column each.
	for _, seed := range []string{
		`INSERT INTO company_config (revision_id, created_at, created_by,
			created_by_kind, source, summary, payload)
		 VALUES ('seed', 1, 'node-a', 'node', 'file', '', '{}')`,
		`INSERT INTO secret_values (name, value, key_id, updated_at, updated_by, source)
		 VALUES ('GITHUB_TOKEN', 'sealed', 'k1', 1, 'ops', 'api')`,
		`INSERT INTO statelog_adoption (started_at, donor, manifest, completed_at, ledger_folded)
		 VALUES (1, 'node-b', '{}', 2, 1)`,
		`INSERT INTO crewlet_events (event_time, event_id, event_type, agent_id, agent_role)
		 VALUES (1, 'e1', 'turn_completed', 'agent-maya', 'Maya')`,
		`INSERT INTO crewlet_events (event_time, event_id, event_type, sender)
		 VALUES (2, 'e2', 'chat_message', 'maya')`,
		`INSERT INTO crewlet_event_parties (party, event_time, event_id) VALUES
			('Maya', 1, 'e1'), ('maya', 1, 'e1'), ('maya', 2, 'e2')`,
	} {
		if _, err := pool.ExecContext(ctx, seed); err != nil {
			t.Fatalf("seed a 0037 database: %v\n%s", err, seed)
		}
	}

	applied, err := db.migrate(ctx)
	if err != nil {
		t.Fatalf("migrate a populated 0037 database: %v", err)
	}
	if !slices.Contains(applied, migration0038) {
		t.Fatalf("applied %v, want %s among them", applied, migration0038)
	}

	var by, kind, payload, operator string
	if err := pool.QueryRowContext(ctx, `SELECT created_by, created_by_kind,
		payload, operator_id FROM company_config WHERE revision_id = 'seed'`).
		Scan(&by, &kind, &payload, &operator); err != nil {
		t.Fatal(err)
	}
	if by != "node-a" || kind != "node" || payload != "{}" || operator != "" {
		t.Errorf("the seeded revision reads (by %q, kind %q, payload %q, "+
			"credential %q), want (node-a, node, {}, none) — 0038 adds a column "+
			"and rewrites no revision", by, kind, payload, operator)
	}
	var value, updatedBy, secretKind, secretOperator string
	if err := pool.QueryRowContext(ctx, `SELECT value, updated_by,
		updated_by_kind, operator_id FROM secret_values WHERE name = 'GITHUB_TOKEN'`).
		Scan(&value, &updatedBy, &secretKind, &secretOperator); err != nil {
		t.Fatal(err)
	}
	if value != "sealed" || updatedBy != "ops" || secretKind != "" || secretOperator != "" {
		t.Errorf("the seeded secret reads (value %q, by %q, kind %q, credential "+
			"%q), want (sealed, ops, none, none)", value, updatedBy, secretKind,
			secretOperator)
	}

	if cols := columnNames(t, db, "statelog_adoption"); slices.Contains(cols, "ledger_folded") {
		t.Errorf("statelog_adoption still carries ledger_folded: %v", cols)
	}
	var donor string
	if err := pool.QueryRowContext(ctx,
		`SELECT donor FROM statelog_adoption WHERE started_at = 1`).Scan(&donor); err != nil {
		t.Fatalf("the adoption row did not survive its column's drop: %v", err)
	}

	type party struct {
		name string
		at   int
		id   string
	}
	rows, err := pool.QueryContext(ctx, `SELECT party, event_time, event_id
		FROM crewlet_event_parties ORDER BY event_time, party`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var parties []party
	for rows.Next() {
		var p party
		if err := rows.Scan(&p.name, &p.at, &p.id); err != nil {
			t.Fatal(err)
		}
		parties = append(parties, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []party{{"agent-maya", 1, "e1"}}; !slices.Equal(parties, want) {
		t.Errorf("the related-agent index holds %v after 0038, want %v — every "+
			"name goes, and each event is filed under its own agent id",
			parties, want)
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
