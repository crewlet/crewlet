package store

import (
	"path"
	"path/filepath"
	"slices"
	"testing"
)

// A MEMORY ROW'S CHANGE SEQUENCE ONLY EVER RISES (node migrations 0041, 0070).
//
// memsync's watermark is over change_seq, so every insert, every vector set in
// place and every update of a mutable row must stamp a value above every value
// before it, across the tables, including after the newest row is deleted:
// these tables are keyed on text, so a rowid IS reused after a delete, and a
// watermark over it skipped the row written into the gap. An in-place change
// that does not travel (a retrieval counter) takes no new value.
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

	// A skill's state and use count are its content: an update restamps it.
	latest := seq("episodes", "e1")
	exec(`UPDATE synthesized_skills SET use_count = use_count + 1 WHERE id = 's1'`)
	if got := seq("synthesized_skills", "s1"); got <= latest {
		t.Errorf("a skill's use counted left its change_seq at %d, behind the latest "+
			"stamp %d — the change would never be carried", got, latest)
	}
}

// AN UPGRADED NODE'S MUTABLE ROWS ARE STAMPED ABOVE THE COUNTER, AND EVERY LATER
// STAMP ABOVE THEM (node migration 0070). The file is built as 0069 left it, so
// 0070 backfills rows a node already held; a node migration cannot drop the
// column in place (schemaFS says why). memsync's backlog reads past a mark that
// starts at 0, and each later change must land past every mark a stamp moved.
//
// Mutation: drop the backfill, its `+ rowid` or any counter move, or have the
// last move count rows rather than take max(rowid) (a marker is deleted for
// that), and a case goes red. Each table's largest rowid exceeds the later
// tables' together, so every move is needed to keep the counter past its stamps.
func TestAnUpgradeStampsTheMutableRowsAboveEveryEarlierAndLaterStamp(t *testing.T) {
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
	const version = "0070_a_mutable_memory_row_carries_its_change_sequence.sql"
	files, err := schemaVersions(EstateNode)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if migrationNumber(name) >= migrationNumber(version) {
			break
		}
		body, err := schemaFS.ReadFile(path.Join("schema", string(EstateNode), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.applyOne(ctx, name, string(body)); err != nil {
			t.Fatalf("build the file as 0069 left it: %v", err)
		}
	}
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatalf("%v\n%s", err, statement)
		}
	}
	exec(`INSERT INTO agent_diary (id, agent_id, kind, content, created_at)
		VALUES ('d1', 'agent', 'diary_long', 'note', 0)`)
	profile := func(subject string) {
		exec(`INSERT INTO counterparty_profiles (observer_handle, subject_handle,
			first_seen_at, last_updated_at, last_corroborated_at)
			VALUES ('eng', ?, 0, 0, 0)`, subject)
	}
	skill := func(id string) {
		exec(`INSERT INTO synthesized_skills (id, agent_handle, name, description,
			content, version, created_at, updated_at, state)
			VALUES (?, 'eng', ?, 'd', 'c', 1, 0, 0, 'active')`, id, id)
	}
	marker := func(agent string) {
		exec(`INSERT INTO agent_onboarding_markers (agent_id, chain_hash, created_at,
			updated_at) VALUES (?, 'h', 0, 0)`, agent)
	}
	for _, subject := range []string{"p1", "p2", "p3", "p4", "p5", "p6"} {
		profile(subject)
	}
	skill("s1")
	skill("s2")
	skill("s3")
	marker("m0")
	marker("m1")
	exec(`DELETE FROM agent_onboarding_markers WHERE agent_id = 'm0'`)
	var before int64
	if err := pool.QueryRowContext(ctx,
		`SELECT last FROM memory_change_sequence`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	done, err := db.migrate(ctx)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !slices.Contains(done, version) {
		t.Fatalf("the migrator applied %v, not 0070", done)
	}
	stamps := func(table string) []int64 {
		t.Helper()
		rows, err := pool.QueryContext(ctx, `SELECT change_seq FROM `+table)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		defer func() { _ = rows.Close() }()
		var out []int64
		for rows.Next() {
			var s int64
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	tables := []string{"counterparty_profiles", "synthesized_skills", "agent_onboarding_markers"}
	held := map[string][]int64{}
	for _, table := range tables {
		got := stamps(table)
		held[table] = got
		for _, s := range got {
			if s <= before {
				t.Errorf("%s: an existing row took change_seq %d, at or below the "+
					"counter's %d — the backlog would never carry it", table, s, before)
			}
		}
		if distinct := slices.Compact(slices.Sorted(slices.Values(got))); len(distinct) != len(got) {
			t.Errorf("%s: existing rows share a change_seq: %v", table, got)
		}
	}
	var counter int64
	if err := pool.QueryRowContext(ctx,
		`SELECT last FROM memory_change_sequence`).Scan(&counter); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		if high := slices.Max(held[table]); counter < high {
			t.Errorf("%s: the counter ended at %d, behind the backfilled %d — "+
				"the next stamp would land at or below a mark", table, counter, high)
		}
	}

	// The next change to each table lands above every stamp it holds.
	exec(`UPDATE counterparty_profiles SET interaction_count = 1 WHERE subject_handle = 'p2'`)
	exec(`UPDATE synthesized_skills SET use_count = 1 WHERE id = 's1'`)
	exec(`UPDATE agent_onboarding_markers SET summary = 's' WHERE agent_id = 'm1'`)
	profile("p7")
	skill("s4")
	marker("m2")
	for _, table := range tables {
		high := slices.Max(held[table])
		changed := 0
		for _, s := range stamps(table) {
			if !slices.Contains(held[table], s) {
				changed++
				if s <= high {
					t.Errorf("%s: a change after the upgrade took change_seq %d, at or "+
						"below the backfilled %d", table, s, high)
				}
			}
		}
		if changed != 2 {
			t.Errorf("%s: an insert and an update after the upgrade left %d new "+
				"stamps, want 2", table, changed)
		}
	}
}
