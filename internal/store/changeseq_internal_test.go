package store

import (
	"path/filepath"
	"testing"
)

// A MEMORY ROW'S CHANGE SEQUENCE ONLY EVER RISES (node migration 0041).
//
// memsync's watermark is over change_seq, so every insert — and every vector
// set in place — must stamp a value above every value before it, across all
// three tables, including after the newest row is deleted: these tables are
// keyed on text, so a rowid IS reused after a delete, and a watermark over it
// skipped the row written into the gap. An in-place change that does not
// travel (a retrieval counter) takes no new value.
func TestEveryMemoryChangeIsStampedAboveEveryOneBeforeIt(t *testing.T) {
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
	if _, err := db.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
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

	seq := func(table, id string) int64 {
		t.Helper()
		var change int64
		if err := pool.QueryRowContext(ctx,
			`SELECT change_seq FROM `+table+` WHERE id = ?`, id).Scan(&change); err != nil {
			t.Fatalf("read %s %s: %v", table, id, err)
		}
		return change
	}
	d3 := seq("agent_diary", "d3")
	d1, d2 := seq("agent_diary", "d1"), seq("agent_diary", "d2")
	e1, v1 := seq("episodes", "e1"), seq("synthesized_skill_versions", "v1")
	if increasing := d1 < d2 && d2 < d3 && d3 < e1 && e1 < v1; !increasing {
		t.Errorf("the stamps are not one increasing sequence across the tables: "+
			"%d, %d, %d, %d, %d", d1, d2, d3, e1, v1)
	}
	high := seq("synthesized_skill_versions", "v1")

	// The newest diary row goes, as the trim would take it, and the next
	// writes follow: every one must land above every value before them,
	// although the note takes the deleted row's rowid again.
	exec(`DELETE FROM agent_diary WHERE id = 'd3'`)
	diary("d4")
	if got := seq("agent_diary", "d4"); got <= high {
		t.Errorf("a note written after the newest was deleted took change_seq %d, "+
			"at or below the earlier high of %d", got, high)
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
