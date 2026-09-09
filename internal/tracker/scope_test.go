package tracker_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY OP DECLARES WHAT IT WRITES.
//
// # The failure this exists to catch
//
// A record's declared scope is what makes "a record this build cannot decode
// blocks the objects it touched" true rather than "blocks the object it
// names". Nine operations in this design write rows for an object other than
// their subject's — a status change rewrites two columns on every dependent, a
// rank move writes every task it placed — and every one of them has to say so.
//
// An op added later that writes a neighbour's row WITHOUT naming it is silent:
// the record applies, the rows land, and the writer's own step-0 probe reports
// the neighbour as clean. A writer then takes the retry-at-zero branch and
// overwrites a mutation no later reprocess can recover. Nothing raises,
// nothing logs, and the only symptom is a row that quietly went backwards.
//
// So this walks the apply cases, runs each against a fixture, and asserts that
// every object row the apply MOVED is one the record's own scope covers.
func TestEveryOpDeclaresItsScope(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		seed   func(*applyHarness)
		record func() tracker.MutationRecord
	}{
		"a task create touches its own task": {
			record: func() tracker.MutationRecord {
				return taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil)
			},
		},
		"a task patch touches its own task": {
			seed: func(h *applyHarness) {
				if _, err := h.apply(
					taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil),
					time.Unix(1_700_000_100, 0).UTC()); err != nil {
					h.t.Fatalf("seed: %v", err)
				}
			},
			record: func() tracker.MutationRecord {
				return taskRecord("t-1", tracker.OpPatch,
					tracker.TaskPatch{Title: ptr("moved")}, nil)
			},
		},
		"a rank move touches every task it places, and says so": {
			seed: func(h *applyHarness) {
				for _, id := range []string{"t-1", "t-2"} {
					if _, err := h.apply(
						taskRecord(id, tracker.OpCreate, newTask(id), nil),
						time.Unix(1_700_000_100, 0).UTC()); err != nil {
						h.t.Fatalf("seed %s: %v", id, err)
					}
				}
			},
			record: func() tracker.MutationRecord {
				return tracker.MutationRecord{
					RecordEnvelope: tracker.RecordEnvelope{
						V: tracker.RecordVersion, OpID: "rank-1",
						Subject: tracker.RankOrderSubject("ENG"),
						Op:      tracker.OpPatch, Writer: "node-a",
						// THE COVERING TERM, not an enumeration: a
						// drag's affected set is the tasks it names
						// PLUS up to a re-spread's worth of
						// neighbours, which is why it states the
						// project rather than a list it could
						// overflow.
						Scope: tracker.ScopeSet{Terms: []tracker.ScopeTerm{
							{Kind: tracker.TermContainer, ID: "ENG"},
						}},
					},
					Mutation: mustJSON(tracker.RankOrder{
						V: 1, Project: "ENG",
						Placements: []tracker.Placement{
							{Task: "t-1", Rank: "a1"},
							{Task: "t-2", Rank: "a2"},
						},
					}),
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newApplyHarness(t)
			if tc.seed != nil {
				tc.seed(h)
			}
			record := tc.record()
			before := h.seq + 1
			if _, err := h.apply(record, time.Unix(1_700_000_200, 0).UTC()); err != nil {
				t.Fatalf("apply: %v", err)
			}
			position := statelog.Position{
				Stream: "CREWLET_TRACKER_LOG", Generation: record.Gen, Seq: before,
			}
			moved := h.movedAt(position.Packed())
			if len(moved) == 0 {
				t.Fatalf("the record moved no object row at all, so this case is "+
					"asserting nothing about %s", record.Op)
			}
			scope := record.Scope.Resolve(record.Subject, "ENG")
			for _, id := range moved {
				touched := statelog.ScopeSet{Paths: []string{
					tracker.ScopeTerm{
						Kind: tracker.TermObject, Container: "ENG", ID: id,
					}.Path(),
				}}
				if !scope.Intersects(touched) {
					t.Errorf("the record on %s moved task %s, which its declared "+
						"scope %v does not cover — a writer probing that task "+
						"would be told it is clean", record.Subject, id, scope.Paths)
				}
			}
		})
	}
}

// movedAt is every task row this position wrote, by either of the two columns
// a write may stamp.
//
// BOTH COLUMNS, because that is the distinction the scope rule turns on: a
// record bumps `version` only on its own subject and stamps `scoped_through`
// on every other row it writes, so reading one of them would miss exactly the
// neighbours this test exists to find.
func (h *applyHarness) movedAt(packed int64) []string {
	h.t.Helper()
	var out []string
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(h.t.Context(),
			`SELECT id FROM tracker_tasks
			 WHERE version = ? OR scoped_through = ? ORDER BY id`, packed, packed)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	}); err != nil {
		h.t.Fatalf("read what the record moved: %v", err)
	}
	return out
}
