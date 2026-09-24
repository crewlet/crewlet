package store

import (
	"path"
	"path/filepath"
	"slices"
	"testing"
)

// migration0031 is the file under test, named once.
const migration0031 = "0031_a_turn_names_its_work_item.sql"

// 0031 APPLIES OVER A POPULATED 0030 DATABASE.
//
// The other cases open a fresh file, where every migration runs over empty
// tables and a statement that breaks on existing rows — a rename the engine
// refuses, a backfill that names a column wrongly — passes. An operator's
// node is never fresh: it upgrades a database holding events that name an item
// only in their payload and episodes filed under the column's old name. So the
// file is brought to exactly 0030, populated in 0030's shapes, and then handed
// to the ordinary migrator.
func TestNode0031AppliesOverAPopulated0030Database(t *testing.T) {
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
	if !slices.Contains(files, migration0031) {
		t.Fatalf("%s is not among the node migrations %v", migration0031, files)
	}
	for _, name := range files {
		if name >= migration0031 {
			break
		}
		body, err := schemaFS.ReadFile(path.Join("schema", string(EstateNode), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.applyOne(ctx, name, string(body)); err != nil {
			t.Fatalf("bring the file to 0030: %v", err)
		}
	}

	// 0030's SHAPES: the item only in the payload, the episode under task_id.
	for _, row := range []struct{ id, payload string }{
		{"named", `{"turn_id":"run-1","work_item":{"backend":"jira","id":"10042","key":"ENG-4","project":"ENG"}}`},
		{"idless", `{"turn_id":"run-2","work_item":{"backend":"native","key":"ENG-5"}}`},
		{"none", `{"turn_id":"run-3"}`},
		{"scalar", `{"turn_id":"run-4","work_item":"native:x"}`},
	} {
		if _, err := pool.ExecContext(ctx, `
			INSERT INTO crewlet_events (event_time, event_id, event_type, source,
				category, summary, actor, tags, payload)
			VALUES (1, ?, 'turn_completed', 'engine', 'lifecycle', '', '', '{}', ?)`,
			row.id, row.payload); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO episodes (id, agent_handle, agent_role, task_id, turn_id,
			started_at, ended_at, plan_summary, task_summary, review_outcome,
			duration_ms, work_key)
		VALUES ('ep-1', 'ada', 'Dev', 'native:task-9', 'run-1', 1, 2, '', '',
			'approved', 1, 'wk-1')`); err != nil {
		t.Fatalf("seed an episode under task_id: %v", err)
	}

	applied, err := db.migrate(ctx)
	if err != nil {
		t.Fatalf("migrate a populated 0030 database: %v", err)
	}
	if !slices.Contains(applied, migration0031) {
		t.Fatalf("applied %v, want %s among them", applied, migration0031)
	}

	want := map[string]string{"named": "jira:10042", "idless": "", "none": "", "scalar": ""}
	for id, item := range want {
		var got string
		if err := pool.QueryRowContext(ctx,
			`SELECT work_item FROM crewlet_events WHERE event_id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != item {
			t.Errorf("%s: work_item = %q after the backfill, want %q", id, got, item)
		}
	}
	var episodeItem string
	if err := pool.QueryRowContext(ctx,
		`SELECT work_item FROM episodes WHERE id = 'ep-1'`).Scan(&episodeItem); err != nil {
		t.Fatalf("read the renamed episode column: %v", err)
	}
	if episodeItem != "native:task-9" {
		t.Errorf("episode work_item = %q, want the value task_id held", episodeItem)
	}
	var indexed int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema
		WHERE type = 'index' AND name = 'crewlet_events_work_item_idx'`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed != 1 {
		t.Error("the item filter's index was not created")
	}
}
