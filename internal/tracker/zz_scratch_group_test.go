package tracker

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

func scratchTry(t *testing.T, db *store.DB, name, statement string, args ...any) {
	t.Helper()
	start := time.Now()
	var rows int
	err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		r, err := tx.QueryContext(t.Context(), statement, args...)
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		for r.Next() {
			rows++
		}
		return r.Err()
	})
	took := time.Since(start)
	if err != nil {
		fmt.Printf("=== %s: ERROR %v\n\n", name, err)
		return
	}
	fmt.Printf("=== %s: ok, %d rows, %s\n", name, rows, took.Round(time.Microsecond))
	for _, line := range explain(t, db, statement, args) {
		fmt.Printf("      %s\n", line)
	}
	fmt.Println()
}

func TestScratchGrouping2(t *testing.T) {
	db := planStore(t)

	// (a) Does an index matching the partition+order kill the sorter?
	w, err := db.Replicated().Writer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{
		`CREATE INDEX zz_group_status_idx ON tracker_tasks
		    (status, updated_at DESC, id) WHERE removed_at IS NULL`,
		`CREATE INDEX zz_group_proj_status_idx ON tracker_tasks
		    (project_key, status, rank, id) WHERE removed_at IS NULL`,
	} {
		if _, err := w.Conn().ExecContext(t.Context(), ddl); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Conn().ExecContext(t.Context(), `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	w.Close()

	scratchTry(t, db, "(a) window with a matching index (workspace)", `
		SELECT id, grp, rn FROM (
			SELECT t.id AS id, t.status AS grp,
			       ROW_NUMBER() OVER (PARTITION BY t.status ORDER BY t.updated_at DESC, t.id) AS rn
			FROM tracker_tasks t
			WHERE t.removed_at IS NULL
		) WHERE rn <= 10 ORDER BY grp, rn`)

	scratchTry(t, db, "(a2) window with a matching index (project)", `
		SELECT id, grp, rn FROM (
			SELECT t.id AS id, t.status AS grp,
			       ROW_NUMBER() OVER (PARTITION BY t.status ORDER BY t.rank, t.id) AS rn
			FROM tracker_tasks t
			WHERE t.removed_at IS NULL AND t.project_key = ?
		) WHERE rn <= 10 ORDER BY grp, rn`, "P01")

	// (b) The N-statement alternative: distinct keys + counts, then one
	// limited page per key.
	start := time.Now()
	var keys []string
	counts := map[string]int{}
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		r, err := tx.QueryContext(t.Context(), `
			SELECT t.status, COUNT(*) FROM tracker_tasks t
			WHERE t.removed_at IS NULL GROUP BY t.status ORDER BY t.status`)
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		for r.Next() {
			var k string
			var n int
			if err := r.Scan(&k, &n); err != nil {
				return err
			}
			keys = append(keys, k)
			counts[k] = n
		}
		return r.Err()
	}); err != nil {
		t.Fatal(err)
	}
	countStep := time.Since(start)
	start = time.Now()
	total := 0
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		for _, k := range keys {
			r, err := tx.QueryContext(t.Context(), `
				SELECT t.id FROM tracker_tasks t
				WHERE t.removed_at IS NULL AND t.status = ?
				ORDER BY t.updated_at DESC, t.id LIMIT 10`, k)
			if err != nil {
				return err
			}
			for r.Next() {
				total++
			}
			if err := r.Err(); err != nil {
				_ = r.Close()
				return err
			}
			_ = r.Close()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("=== (b) N statements: %d keys in %s, %d rows in %s, total %s\n\n",
		len(keys), countStep.Round(time.Microsecond), total,
		time.Since(start).Round(time.Microsecond), (countStep + time.Since(start)).Round(time.Microsecond))

	scratchTry(t, db, "(b2) one per-key page, planned", `
		SELECT t.id FROM tracker_tasks t
		WHERE t.removed_at IS NULL AND t.status = ?
		ORDER BY t.updated_at DESC, t.id LIMIT 10`, "todo")

	// (c) An integer-division date bucket: zone anchored, DST-blind.
	scratchTry(t, db, "(c) integer-division week bucket", `
		SELECT (t.due_at - ?) / ? AS grp, COUNT(*) FROM tracker_tasks t
		WHERE t.removed_at IS NULL AND t.due_at IS NOT NULL
		GROUP BY grp ORDER BY grp LIMIT 20`, int64(0), int64(604800000000))

	// (d) The whole-corpus fold: rows + key, no window, ordered by (key, sort).
	scratchTry(t, db, "(d) ordered by (group, sort) for an in-Go fold", `
		SELECT t.status, t.id FROM tracker_tasks t
		WHERE t.removed_at IS NULL
		ORDER BY t.status, t.updated_at DESC, t.id`)

	// (e) The same, but bounded by the ceiling.
	scratchTry(t, db, "(e) in-Go fold bounded at the ceiling", `
		SELECT t.status, t.id FROM tracker_tasks t
		WHERE t.removed_at IS NULL
		ORDER BY t.status, t.updated_at DESC, t.id LIMIT ?`, GroupByRowCeiling)
}
