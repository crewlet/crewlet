package code

import (
	"database/sql"
	"testing"
)

// A RUN'S WINDOW REACHES NO ROW PAST ITS PAGE'S BYTES: the span of pushes a
// page of a run's pushes may read is weighed by what each row holds, as the
// index carries it, and ends at the first push whose weight fills the page —
// counting what the page already holds — or at the rows it has left; and its
// last push is the one the statement that looks the span's rows up to test the
// run stops at, so that statement reads none a page would not count. The
// answer cannot show it: a page that looked up a few rows more would still end
// where the weights say. So this holds the span itself, where its pushes weigh
// less than the page's bytes left (one aggregate) and where they weigh more
// (the walk to the push that fills it).
//
// Mutations: weigh the walked pushes as nothing (runWeights adding 0 to the
// page) — the span runs to the page's row limit, past the push that filled
// its bytes, red; take the aggregate's count past the page's bytes as the
// span (runSpanOf never walking) — the same, red; read the last push one
// further (runLastSQL's offset the span's count) — the span ends a push late,
// red.
func TestARunsWindowReachesNoRowPastItsPagesBytes(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-1", "app"))
	// FIVE PUSHES at positions 1 to 5, weighing 100 to 500 bytes.
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM k WHERE i < 5)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, commands, received_at,
			row_bytes)
		SELECT printf('p%d', i), 'repo-1', i, i, 'ada', 'operator', '[]', i, i * 100 FROM k`)
	for name, tc := range map[string]struct {
		reads    page
		position int64
		pushID   string
		want     runSpan
	}{
		"its bytes, from the run's launch": {reads: page{maxRows: 10, maxB: 550}, position: 1,
			want: runSpan{pushes: 3, weight: 600, lastPosition: 3, lastID: "p3"}},
		"its rows, from the run's launch": {reads: page{maxRows: 2, maxB: MaxPageBytes}, position: 1,
			want: runSpan{pushes: 2, weight: 300, lastPosition: 2, lastID: "p2"}},
		"every push left, under its bytes": {reads: page{maxRows: 10, maxB: MaxPageBytes}, position: 1,
			want: runSpan{pushes: 5, weight: 1500, lastPosition: 5, lastID: "p5"}},
		// A PAGE THAT HOLDS ROWS ALREADY, of an earlier repository, weighs
		// what it holds: one push more fills it.
		"its bytes, after a cursor and what the page holds": {reads: page{rows: 1, bytes: 450, maxRows: 10,
			maxB: 550}, position: 1, pushID: "p1",
			want: runSpan{pushes: 1, weight: 200, lastPosition: 2, lastID: "p2"}},
		"nothing after the last push": {reads: page{maxRows: 10, maxB: MaxPageBytes}, position: 5,
			pushID: "p5"},
	} {
		var span runSpan
		if err := e.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
			var err error
			span, err = runSpanOf(t.Context(), tx, "repo-1", tc.position, tc.pushID, tc.reads)
			return err
		}); err != nil {
			t.Fatalf("%s: runSpanOf: %v", name, err)
		}
		if span != tc.want {
			t.Errorf("%s: the span is %+v, want %+v: no push past the one that fills the page", name, span,
				tc.want)
		}
	}
}
