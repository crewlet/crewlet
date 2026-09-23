package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// EVERY POINT READ OF THE LOG IS AN INDEX SEARCH, AND EVERY PURGE STATEMENT
// SEEKS RATHER THAN WALKS.
//
// A plan is the one property of a query whose regression looks like nothing
// at all: the answer is the same, the tests pass, and the read that served a
// pasted link in a tenth of a millisecond serves it in two, and in twenty on a
// log ten times the size. The statements asked here are the ones the code
// runs, by name, so a rewrite of one is a rewrite this test reads.
//
// The obvious way to ask for an id's newest row — ORDER BY event_time DESC
// LIMIT 1 — is what regressed: measured, it planned as a SCAN of the primary
// key's index for ByID and as a MULTI-INDEX AND with a sorter for a phase
// record's rows. Neither statement orders now, and this holds them to that.
func TestAPointReadIsAnIndexSearch(t *testing.T) {
	t.Parallel()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "plans.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id, at := seedPlans(t, db, 2000)
	cutoff := EncodeTime(now())

	idSearch := "SEARCH crewlet_events USING INDEX crewlet_events_id_idx (event_id=?)"
	for _, tc := range []struct {
		name  string
		query string
		args  []any
		want  string
	}{
		{"ByID", pointReadSQL + byIDWhere, []any{id}, idSearch},
		{"ByKey", pointReadSQL + byKeyWhere, []any{EncodeTime(at), id}, "SEARCH crewlet_events"},
		{"a phase record's rows", phaseRecordRowSQL, []any{id, phaseCompleted}, idSearch},
		{"a part's rows", phaseRecordPartSQL, []any{id, phasePart}, idSearch},
		{"the purge's candidates", purgeCandidatesSQL, []any{cutoff, EventPurgeBatch}, "SEARCH crewlet_events"},
		{"the party index's purge", partyPurgeSQL, []any{cutoff, eventPartyPurgeBatch}, "SEARCH crewlet_event_parties"},
		{"a purge batch's delete", deleteBatchSQL(3), []any{cutoff, 1, 2, 3}, "SEARCH crewlet_events"},
	} {
		plan := planOf(t, db, tc.query, tc.args...)
		if !strings.Contains(plan, tc.want) {
			t.Errorf("%s plans as %q; want %q", tc.name, plan, tc.want)
		}
		for _, walk := range []string{"SCAN", "MULTI-INDEX", "SORTER"} {
			if strings.Contains(plan, walk) {
				t.Errorf("%s plans as %q, which walks rows (%s) rather than seeking them", tc.name, plan, walk)
			}
		}
	}

	// THE CONTROL: a read no index can serve plans as a SCAN, so the
	// assertions above are able to see one.
	if plan := planOf(t, db, "SELECT event_id FROM crewlet_events WHERE payload = ?", "{}"); !strings.Contains(plan, "SCAN") {
		t.Errorf("a read of an unindexed column plans as %q; the plan check cannot see a scan", plan)
	}
}

// seedPlans writes n phase records, a part and a row of another type beside
// each, and a party row for each, in one transaction: a log with enough rows
// of each kind that the planner has a real choice, at a cost a test can pay.
// It returns one phase record's id and instant.
func seedPlans(t *testing.T, db *DB, n int) (string, time.Time) {
	t.Helper()
	base := now().Add(-time.Hour)
	var id string
	err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range n {
			at := EncodeTime(base.Add(time.Duration(i) * time.Millisecond))
			record := uuid.NewString()
			if i == n/2 {
				id = record
			}
			for _, row := range [][2]string{
				{record, phaseCompleted}, {uuid.NewString(), phasePart}, {uuid.NewString(), "task_assigned"},
			} {
				if _, err := tx.ExecContext(t.Context(),
					`INSERT INTO crewlet_events (event_time, event_id, event_type, payload) VALUES (?, ?, ?, '{}')`,
					at, row[0], row[1]); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(t.Context(),
				`INSERT INTO crewlet_event_parties (party, event_time, event_id) VALUES ('lead', ?, ?)`,
				at, record); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return id, base.Add(time.Duration(n/2) * time.Millisecond)
}

// planOf is a statement's EXPLAIN QUERY PLAN, its detail column joined.
func planOf(t *testing.T, db *DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.SQL().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for rows.Next() {
		cells := make([]any, len(cols))
		into := make([]any, len(cols))
		for i := range cells {
			into[i] = &cells[i]
		}
		if err := rows.Scan(into...); err != nil {
			t.Fatal(err)
		}
		switch detail := cells[len(cells)-1].(type) {
		case string:
			steps = append(steps, detail)
		case []byte:
			steps = append(steps, string(detail))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(steps, " | ")
}
