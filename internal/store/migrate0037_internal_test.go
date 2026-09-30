package store

import (
	"encoding/json"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// migration0037 is the file under test, named once.
const migration0037 = "0037_the_runtime_audit_tags_its_history.sql"

// 0037 GIVES THE RUNTIME AUDIT'S OLDER ROWS EXACTLY THE TAGS THE WRITER NOW
// WRITES.
//
// A row the previous build stored carries every tag but the four this build
// promoted (node, actor_seat, tool, dir), so beside a new row the Audit log drew
// a token's name where it now draws the person and "tool call" where it names
// the tool, and a backup's history row named no host. After the backfill a row
// must read back as if this build had written it — the same map ExtractTags
// derives from the same stored event, built from the real types so a renamed
// wire field fails here — while a tag already present is left alone and no
// other event type is touched.
func TestNode0037TagsTheRuntimeAuditAnUpgradeHolds(t *testing.T) {
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

	backup := events.New(types.NewBackupRequested(types.BackupRequested{
		OperatorID: "maya-laptop", ActorSeat: "maya", Dir: "/var/backups/one",
		Outcome: types.AuditFailed, Failed: true,
	}), events.TraceContext{})
	backup.Node = "node-b"
	acted := events.New(types.NewOperatorActed(types.OperatorActed{
		OperatorID: "maya-laptop", ActorSeat: "maya", Transport: types.TransportAct,
		Tool: "save_page", Outcome: types.AuditRefused, Refusal: "conflict",
	}), events.TraceContext{})
	acted.Node = "node-a"
	unbound := events.New(types.NewOperatorActed(types.OperatorActed{
		OperatorID: "ci", Transport: types.TransportMCP, Tool: "list_work_items",
		Outcome: types.AuditApplied,
	}), events.TraceContext{})
	unbound.Node = "node-a"

	promoted := []string{"node", "actor_seat", "tool", "dir"}
	// THE PREVIOUS BUILD'S TAGS: everything ExtractTags derives but the four.
	oldTags := func(raw []byte) map[string]string {
		tags := ExtractTags(raw)
		for _, key := range promoted {
			delete(tags, key)
		}
		return tags
	}
	type seeded struct {
		id, eventType string
		raw           []byte
		tags          map[string]string
	}
	var rows []seeded
	for _, ev := range []*events.Event{backup, acted, unbound} {
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, seeded{ev.ID.String(), ev.Type, raw, oldTags(raw)})
	}
	// A TAG ALREADY PRESENT IS NOT OVERWRITTEN, and ANOTHER TYPE carrying the
	// same fields in its payload is not touched at all.
	kept := map[string]string{"actor_seat": "already", "failed": "true"}
	rows = append(rows,
		seeded{"kept", "backup_requested",
			[]byte(`{"node":"node-c","actor_seat":"maya","dir":"/b"}`), kept},
		seeded{"other", "turn_completed",
			[]byte(`{"node":"node-c","actor_seat":"maya","tool":"x","dir":"/y"}`), map[string]string{}},
	)
	for i, row := range rows {
		tags, err := json.Marshal(row.tags)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, `
			INSERT INTO crewlet_events (event_time, event_id, event_type, source,
				category, summary, actor, tags, payload)
			VALUES (?, ?, ?, 'operator', 'operator', '', '', ?, ?)`,
			i+1, row.id, row.eventType, string(tags), string(row.raw)); err != nil {
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
	for _, row := range rows[:3] {
		if got, want := read(row.id), ExtractTags(row.raw); !maps.Equal(got, want) {
			t.Errorf("%s: tags %v after the backfill, want what the writer writes: %v",
				row.eventType, got, want)
		}
	}
	if got, want := read("kept"), map[string]string{
		"actor_seat": "already", "failed": "true", "node": "node-c", "dir": "/b",
	}; !maps.Equal(got, want) {
		t.Errorf("a row already tagged: %v, want %v", got, want)
	}
	if got := read("other"); len(got) != 0 {
		t.Errorf("another event type was tagged: %v", got)
	}
}
