package store

import (
	"encoding/json"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"testing"
)

// migration0037 is the file under test, named once.
const migration0037 = "0037_the_runtime_audit_tags_its_history.sql"

// 0037 FILLS THE FOUR TAGS IT NAMES FROM THE PAYLOAD, ONLY WHERE ABSENT, AND
// ONLY ON THE RUNTIME AUDIT'S TWO TYPES.
//
// The file is history: it shipped on a line whose writer promoted `node`,
// `actor_seat`, `tool` and `dir`, and it backfilled the rows that writer's
// predecessor had stored. This build's writer promotes a different set —
// no `actor_seat`, since a person bound to a seat is recorded AS the seat,
// and the author's kind and credential beside it (see store.tagKeys) — so
// what is certified here is the migration's own contract rather than a
// match with today's writer, which it was never written against: each of
// its four tags is copied from a non-empty top-level payload string, a tag
// already present is left alone, and no other event type is touched. A
// fresh store holds no row for it to touch at all.
func TestNode0037FillsTheFourTagsItNames(t *testing.T) {
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
	if !slices.Contains(files, migration0037) {
		t.Fatalf("%s is not among the node migrations %v", migration0037, files)
	}
	for _, name := range files {
		if name >= migration0037 {
			break
		}
		body, err := schemaFS.ReadFile(path.Join("schema", string(EstateNode), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.applyOne(ctx, name, string(body)); err != nil {
			t.Fatalf("bring the file to 0036: %v", err)
		}
	}

	type seeded struct {
		id, eventType, payload string
		tags                   map[string]string
		want                   map[string]string
	}
	rows := []seeded{
		{"backup", "backup_requested",
			`{"node":"node-b","actor_seat":"maya","dir":"/var/backups/one","failed":true}`,
			map[string]string{"failed": "true"},
			map[string]string{"failed": "true", "node": "node-b", "actor_seat": "maya",
				"dir": "/var/backups/one"}},
		{"acted", "operator_acted",
			`{"node":"node-a","actor_seat":"maya","tool":"save_page","failed":true}`,
			map[string]string{"failed": "true"},
			map[string]string{"failed": "true", "node": "node-a", "actor_seat": "maya",
				"tool": "save_page"}},
		// A FIELD THE PAYLOAD DOES NOT CARRY AS A NON-EMPTY STRING stays
		// absent, as the writer leaves it.
		{"unbound", "operator_acted",
			`{"node":"node-a","actor_seat":"","tool":"list_work_items","dir":7}`,
			map[string]string{},
			map[string]string{"node": "node-a", "tool": "list_work_items"}},
		// A TAG ALREADY PRESENT IS NOT OVERWRITTEN.
		{"kept", "backup_requested",
			`{"node":"node-c","actor_seat":"maya","dir":"/b"}`,
			map[string]string{"actor_seat": "already", "failed": "true"},
			map[string]string{"actor_seat": "already", "failed": "true", "node": "node-c",
				"dir": "/b"}},
		// AND ANOTHER TYPE carrying the same fields is not touched at all.
		{"other", "turn_completed",
			`{"node":"node-c","actor_seat":"maya","tool":"x","dir":"/y"}`,
			map[string]string{}, map[string]string{}},
	}
	for i, row := range rows {
		tags, err := json.Marshal(row.tags)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, `
			INSERT INTO crewlet_events (event_time, event_id, event_type, source,
				category, summary, actor, tags, payload)
			VALUES (?, ?, ?, 'operator', 'operator', '', '', ?, ?)`,
			i+1, row.id, row.eventType, string(tags), row.payload); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}

	applied, err := db.migrate(ctx)
	if err != nil {
		t.Fatalf("migrate a populated 0036 database: %v", err)
	}
	if !slices.Contains(applied, migration0037) {
		t.Fatalf("applied %v, want %s among them", applied, migration0037)
	}

	read := func(id string) map[string]string {
		var raw string
		if err := pool.QueryRowContext(ctx,
			`SELECT tags FROM crewlet_events WHERE event_id = ?`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var tags map[string]string
		if err := json.Unmarshal([]byte(raw), &tags); err != nil {
			t.Fatalf("%s: tags %q do not decode: %v", id, raw, err)
		}
		return tags
	}
	for _, row := range rows {
		if got := read(row.id); !maps.Equal(got, row.want) {
			t.Errorf("%s (%s): tags %v after the backfill, want %v",
				row.id, row.eventType, got, row.want)
		}
	}
}
