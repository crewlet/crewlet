package learning

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// plan runs EXPLAIN QUERY PLAN and returns the detail lines.
func plan(t *testing.T, db *store.DB, statement string, args ...any) []string {
	t.Helper()
	rows, err := db.SQL().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+statement, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, parent, notused int64
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan a plan row: %v", err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain: %v", err)
	}
	return out
}

func planStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "plan.db"),
		store.Options{EmbeddingDim: 4})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// RECALL SCANS ONE SEAT, NEVER THE TABLE — and the query PLAN is where that is
// decided, not the WHERE clause that describes it.
//
// schema/node/0002 states the rule this protects: the per-seat index is "what
// keeps that scan over one seat's thousands of rows rather than the whole
// table". Windowing put that within one keyword of being lost. Written as
// `JOIN ord … GROUP BY e.id`, which is the obvious shape and the one
// internal/search uses for the same arithmetic over its own tables, SQLite
// takes the PRIMARY KEY index to satisfy the grouping and walks every episode
// in the file in id order — on a node running twenty seats, twenty times the
// work, with every test still green and every answer still correct.
//
// So the assertion is on the plan itself. An `episodes` line that is a SCAN
// rather than a SEARCH is that regression, whatever produced it.
func TestRecallScansOneSeatRatherThanTheTable(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	probe, err := db.EncodeVector([]float32{1, 0, 0, 0})
	if err != nil {
		t.Fatalf("EncodeVector: %v", err)
	}
	width := len(probe)

	lines := plan(t, db, recallStatement([]Kind{KindRaw}),
		1, probe, "ceo", width, width, width, probe, "ceo", width, 0.7, 5)

	seatScoped, longRowsSought := false, false
	for _, line := range lines {
		// A SEARCH names an index and visits the rows it points at; a
		// SCAN walks everything. The outer fetch-by-id is a SEARCH, the
		// candidate pass must be one too.
		if strings.HasPrefix(line, "SCAN") && strings.Contains(line, "episodes") {
			t.Errorf("the recall plan walks the whole table: %q\n(full plan: %v)",
				line, lines)
		}
		if strings.Contains(line, "episodes_agent_ended_at_idx") {
			seatScoped = true
		}
		if strings.Contains(line, "episodes_agent_windows_idx") {
			longRowsSought = true
		}
	}
	if !seatScoped {
		t.Errorf("no step of the recall plan uses episodes_agent_ended_at_idx, so "+
			"nothing scopes the scan to one seat\n(full plan: %v)", lines)
	}
	// THE MULTI-WINDOW ROWS ARE SOUGHT, NOT FILTERED OUT OF THE REST. That
	// is what keeps the ordinal subquery off the one-window rows, which are
	// almost all of them: folded back into a single branch, every row pays a
	// scan of `ord` to rediscover that it has one window, and one 150-window
	// episode takes the seat's recall from 15 ms to 66 ms at 2 000 rows
	// (measured — see the comment at the call site). Nothing else can notice
	// that: the answers stay correct and the suite stays green.
	if !longRowsSought {
		t.Errorf("no step of the recall plan uses episodes_agent_windows_idx, so "+
			"the ordinal path is not confined to the rows that need it\n"+
			"(full plan: %v)", lines)
	}
}

// THE ORDINAL COUNT IS A SEEK, NOT A SCAN, which is the whole reason node
// migration 0030 ships a partial index for it.
//
// Recall asks this before it can generate one ordinal per window. Answered
// over the per-seat time index it is a walk of every one of the seat's rows to
// read one integer — the same work the recall itself does, doubling a read
// that runs at the start of every turn to learn a number that is 1 for almost
// every company. Over the partial index it is one seek, and the index is
// nearly empty because a summary short enough to be one window is almost every
// summary.
func TestTheOrdinalCountIsAnsweredFromThePartialIndex(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	lines := plan(t, db, widestWindowStatement, "ceo", 16)
	for _, line := range lines {
		if strings.Contains(line, "episodes_agent_windows_idx") {
			return
		}
	}
	t.Errorf("the ordinal-count read does not use episodes_agent_windows_idx, so "+
		"every recall pays a per-seat scan to learn a number that is almost "+
		"always 1\n(full plan: %v)", lines)
}
