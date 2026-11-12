package gitlet

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

// seedEnded writes an ended entry of repo, its ejection weighing weighs bytes,
// and seedLandingBelow a landing at position below a green row at 1000.
func (e *estate) seedEnded(repo, id string, endedAt, weighs int64) {
	e.t.Helper()
	e.exec(`INSERT INTO code_queue (repo_id, entry_id, target_ref, change_id, seq, head_oid, method,
		delete_source, close_task, enqueued_by, enqueued_kind, enqueued_operator, state,
		enqueued_at, ended_at, ended_position, end_reason, ejected, ejected_bytes)
		VALUES (?, ?, 'refs/heads/main', ?, 1, ?, 'merge', 0, 0, 'dev', 'agent', '', 'ejected', 1, ?, 2,
			'conflict', '', ?)`, repo, id, "change-"+id, oid("B"), endedAt, weighs)
}

func (e *estate) seedLandingBelow(repo string, position, at int64) {
	e.t.Helper()
	e.exec(`INSERT INTO code_landings (repo_id, target_ref, position, ordinal, kind, base_oid, commit_oid,
		merged_by, at) VALUES (?, 'refs/heads/main', ?, 1, 'push', ?, ?, 'founder', ?)`,
		repo, position, oid("A"), oid("B"), at)
	e.exec(`INSERT INTO code_green (repo_id, target_ref, commit_oid, position, at)
		VALUES (?, 'refs/heads/main', ?, 1000, ?) ON CONFLICT (repo_id, target_ref) DO NOTHING`,
		repo, oid("B"), at)
}

// A QUEUE'S HISTORY IS SWEPT BY ROWS AND BY BYTES, AND CUT AT A KEY: the walk
// takes ended entries oldest first by (ended_at, entry_id) while fewer than
// its rows and while what it took weighs under its bytes — the first whatever
// it weighs, and at most one past — takes no landing once its entries were
// cut by either, and the sweep removes exactly the entries up to the key it
// cut at, a tie of instants split by the entries' ids, never by the order the
// rows were written in.
//
// Mutation: weigh nothing (drop the byte bound from endedWithin) — the byte
// cases take every entry, red; take landings after a cut (drop `|| more` from
// queueHistoryIn's return) — "bytes cut" takes the landing, red; remove the
// whole instant the cut ends in (drop `AND (ended_at < ? OR entry_id <= ?)`
// from sweepEndedSQL) — "a tie split by id" removes all four, red.
func TestAQueuesHistoryIsSweptByRowsAndBytesAndCutAtAKey(t *testing.T) {
	t.Parallel()
	const cutoff = 100
	type entry struct {
		id     string
		at     int64
		weighs int64
	}
	cases := []struct {
		name    string
		entries []entry
		landing bool
		rows    int
		bytes   int64
		// wantSwept is the ids the sweep removes, in key order.
		wantSwept   []string
		wantLanding bool
		wantMore    bool
	}{
		{name: "bytes cut", entries: []entry{{"a", 1, 1000}, {"b", 2, 1000}, {"c", 3, 1000}, {"d", 4, 1000},
			{"e", 5, 1000}}, landing: true, rows: 100, bytes: 2500,
			wantSwept: []string{"a", "b", "c"}, wantMore: true},
		{name: "the first whatever it weighs", entries: []entry{{"a", 1, 5000}, {"b", 2, 10}}, rows: 100,
			bytes: 2500, wantSwept: []string{"a"}, wantMore: true},
		{name: "rows cut", entries: []entry{{"a", 1, 0}, {"b", 2, 0}, {"c", 3, 0}, {"d", 4, 0}}, landing: true,
			rows: 3, bytes: 2500, wantSwept: []string{"a", "b", "c"}, wantMore: true},
		{name: "a tie split by id", entries: []entry{{"d", 7, 0}, {"c", 7, 0}, {"b", 7, 0}, {"a", 7, 0}},
			rows: 2, bytes: 2500, wantSwept: []string{"a", "b"}, wantMore: true},
		{name: "everything within", entries: []entry{{"a", 1, 1000}, {"b", 2, 1000}, {"late", cutoff, 0}},
			landing: true, rows: 100, bytes: 2500, wantSwept: []string{"a", "b"}, wantLanding: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := openApplyEstate(t)
			const repo = "repo-1"
			for _, x := range tc.entries {
				e.seedEnded(repo, x.id, x.at, x.weighs)
			}
			if tc.landing {
				e.seedLandingBelow(repo, 500, 1)
			}
			ctx := context.Background()
			var (
				cut     historyCut
				more    bool
				removed int
			)
			if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
				var err error
				if cut, more, err = queueHistoryIn(ctx, tx, repo, cutoff, tc.rows, tc.bytes); err != nil {
					return err
				}
				removed, err = sweepQueueHistory(ctx, tx, repo, cutoff, tc.rows, tc.bytes)
				return err
			}); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if more != tc.wantMore || cut.entries != len(tc.wantSwept) || (len(cut.landings) > 0) != tc.wantLanding {
				t.Fatalf("the cut took %d entries and %d landings (more %v), want %d entries, a landing %v "+
					"(more %v)", cut.entries, len(cut.landings), more, len(tc.wantSwept), tc.wantLanding, tc.wantMore)
			}
			if removed != cut.rows() {
				t.Fatalf("the sweep removed %d rows, and the cut counted %d", removed, cut.rows())
			}
			var kept []string
			for _, x := range tc.entries {
				kept = append(kept, x.id)
			}
			kept = slices.DeleteFunc(kept, func(id string) bool { return slices.Contains(tc.wantSwept, id) })
			slices.Sort(kept)
			if got := e.idsOf(repo); !slices.Equal(got, kept) {
				t.Fatalf("the sweep kept entries %v, want %v", got, kept)
			}
			if left := e.num(`SELECT COUNT(*) FROM code_landings WHERE repo_id = ?`, repo); tc.landing &&
				(left == 0) != tc.wantLanding {
				t.Fatalf("%d landings below the green row are left, want swept %v", left, tc.wantLanding)
			}
		})
	}
}

// idsOf is repo's queue entries' ids, in key order.
func (e *estate) idsOf(repo string) []string {
	e.t.Helper()
	var ids []string
	if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(context.Background(),
			`SELECT entry_id FROM code_queue WHERE repo_id = ? ORDER BY entry_id`, repo)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	}); err != nil {
		e.t.Fatalf("read %s's entries: %v", repo, err)
	}
	return ids
}
