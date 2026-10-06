package store

import (
	"path"
	"path/filepath"
	"slices"
	"testing"
)

// migration0041 is the file under test, named once.
const migration0041 = "0041_a_memory_row_carries_its_change_sequence.sql"

// 0041 NUMBERS THE MEMORY ROWS AN UPGRADE HOLDS SO A WATERMARK STAYS VALID.
//
// Each existing row of the three tables takes its rowid as its change
// sequence and the counter starts at the largest rowid any of them holds — so
// a mark taken over the rowids before the migration still means "everything at
// or below me was carried", and every row written after it, and every vector
// set after it, comes out above every value before it. An in-place change that
// does not travel (a retrieval counter) takes no new value.
func TestNode0041NumbersEveryMemoryRowAndEveryLaterChangeAboveThem(t *testing.T) {
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
			t.Fatalf("bring the file to 0040: %v", err)
		}
	}
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatalf("%v\n%s", err, statement)
		}
	}
	diary := func(id string) {
		exec(`INSERT INTO agent_diary (id, agent_id, kind, content, created_at)
			VALUES (?, 'agent', 'diary_long', 'note', 0)`, id)
	}
	diary("d1")
	diary("d2")
	diary("d3")
	exec(`INSERT INTO episodes (id, agent_handle, agent_role, turn_id, started_at,
		ended_at, plan_summary, task_summary, review_outcome, duration_ms)
		VALUES ('e1', 'eng', 'Engineer', 't', 0, 0, 'did', 'woken', 'done', 1)`)
	exec(`INSERT INTO synthesized_skills (id, agent_handle, name, description,
		content, version, created_at, updated_at, state)
		VALUES ('s1', 'eng', 'n', 'd', 'c', 1, 0, 0, 'active')`)
	exec(`INSERT INTO synthesized_skill_versions (id, skill_id, agent_handle, name,
		description, content, version, refinement_kind, archived_at)
		VALUES ('v1', 's1', 'eng', 'n', 'd', 'c', 1, 'seed', 0)`)

	applied, err := db.migrate(ctx)
	if err != nil {
		t.Fatalf("migrate a populated 0040 database: %v", err)
	}
	if !slices.Contains(applied, migration0041) {
		t.Fatalf("applied %v, want %s among them", applied, migration0041)
	}

	seq := func(table, id string) int64 {
		t.Helper()
		var change int64
		if err := pool.QueryRowContext(ctx,
			`SELECT change_seq FROM `+table+` WHERE id = ?`, id).Scan(&change); err != nil {
			t.Fatalf("read %s %s: %v", table, id, err)
		}
		return change
	}
	for table, ids := range map[string][]string{
		"agent_diary": {"d1", "d2", "d3"}, "episodes": {"e1"},
		"synthesized_skill_versions": {"v1"},
	} {
		for _, id := range ids {
			var rowid, change int64
			if err := pool.QueryRowContext(ctx,
				`SELECT rowid, change_seq FROM `+table+` WHERE id = ?`, id).
				Scan(&rowid, &change); err != nil {
				t.Fatal(err)
			}
			if change != rowid {
				t.Errorf("%s %s: change_seq = %d, want its rowid %d — a mark "+
					"taken over the rowids before the upgrade would no longer "+
					"describe what it carried", table, id, change, rowid)
			}
		}
	}

	// The newest diary row goes, as the trim would take it, and the next
	// writes follow: every one must land above every value before the
	// upgrade (3, the largest rowid any of the tables held).
	exec(`DELETE FROM agent_diary WHERE id = 'd3'`)
	diary("d4")
	if got := seq("agent_diary", "d4"); got <= 3 {
		t.Errorf("a note written after the newest was deleted took change_seq %d, "+
			"at or below the pre-upgrade high of 3", got)
	}
	exec(`INSERT INTO episodes (id, agent_handle, agent_role, turn_id, started_at,
		ended_at, plan_summary, task_summary, review_outcome, duration_ms)
		VALUES ('e2', 'eng', 'Engineer', 't2', 0, 0, 'did', 'woken', 'done', 1)`)
	exec(`INSERT INTO synthesized_skill_versions (id, skill_id, agent_handle, name,
		description, content, version, refinement_kind, archived_at)
		VALUES ('v2', 's1', 'eng', 'n', 'd', 'c', 2, 'refine_skill_tool', 0)`)
	d4, e2, v2 := seq("agent_diary", "d4"), seq("episodes", "e2"),
		seq("synthesized_skill_versions", "v2")
	if d4 >= e2 || e2 >= v2 {
		t.Errorf("the counter is not one increasing sequence across the tables: "+
			"%d, %d, %d", d4, e2, v2)
	}

	// A vector set in place is a change the changelog carries; a
	// retrieval counter moving is not.
	exec(`UPDATE agent_diary SET embedding = x'0000803f', embedding_model = 'm'
		WHERE id = 'd1'`)
	if got := seq("agent_diary", "d1"); got <= v2 {
		t.Errorf("a vector set on a note left its change_seq at %d, behind the "+
			"latest stamp %d — the fill would never be carried", got, v2)
	}
	exec(`UPDATE episodes SET embedding = x'0000803f', embedding_model = 'm'
		WHERE id = 'e1'`)
	if got := seq("episodes", "e1"); got <= seq("agent_diary", "d1") {
		t.Errorf("a vector set on an episode left its change_seq at %d", got)
	}
	before := seq("agent_diary", "d2")
	exec(`UPDATE agent_diary SET retrieval_count = retrieval_count + 1 WHERE id = 'd2'`)
	if got := seq("agent_diary", "d2"); got != before {
		t.Errorf("a retrieval counter moving restamped the note (%d → %d), "+
			"which would republish bookkeeping every cycle", before, got)
	}
}
