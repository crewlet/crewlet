package store

import (
	"path"
	"path/filepath"
	"slices"
	"testing"
)

// migration0042 is the file under test, named once.
const migration0042 = "0042_an_episode_keeps_what_it_was_asked.sql"

// 0042 GIVES EVERY EPISODE AN ASK, empty for the rows an upgrade holds: what
// they were asked was never stored, so they are filled from what they do
// store, and nothing about the row is otherwise touched.
func TestNode0042GivesEveryEpisodeAnEmptyAsk(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, err := openPrepared(ctx, filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("openPrepared: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	db := &DB{sql: pool, estate: EstateNode}
	if err := db.createMigrationLedger(ctx); err != nil {
		t.Fatal(err)
	}
	files, err := schemaVersions(EstateNode)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(files, migration0042) {
		t.Fatalf("%s is not among the node migrations %v", migration0042, files)
	}
	for _, name := range files {
		if name >= migration0042 {
			break
		}
		body, err := schemaFS.ReadFile(path.Join("schema", string(EstateNode), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.applyOne(ctx, name, string(body)); err != nil {
			t.Fatalf("bring the file to 0041: %v", err)
		}
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO episodes (id, agent_handle,
		agent_role, turn_id, started_at, ended_at, plan_summary, task_summary,
		review_outcome, duration_ms)
		VALUES ('e1', 'eng', 'Engineer', 't', 0, 0, 'did', 'woken', 'done', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.migrate(ctx); err != nil {
		t.Fatalf("migrate a populated 0041 database: %v", err)
	}
	var ask, plan string
	if err := pool.QueryRowContext(ctx,
		`SELECT ask, plan_summary FROM episodes WHERE id = 'e1'`).Scan(&ask, &plan); err != nil {
		t.Fatal(err)
	}
	if ask != "" || plan != "did" {
		t.Errorf("after 0042 the row holds ask %q and plan %q, want no ask and its plan", ask, plan)
	}
}
