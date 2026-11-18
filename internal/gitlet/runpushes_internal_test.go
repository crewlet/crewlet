package gitlet

import (
	"database/sql"
	"testing"
)

// A RUN'S WINDOW REACHES NO ROW PAST ITS PAGE'S BYTES: the span a page may
// read is weighed by each row's indexed weight and ends at the first push that
// fills the page (counting what it already holds) or at the rows left, and the
// row lookup stops at that push. The answer cannot show it, so this holds the
// span itself, where its pushes weigh less than the bytes left (one aggregate)
// and more (the walk to the push that fills it).
//
// Mutations, each red: runWeights adding 0 (the span runs to the row limit);
// runSpanOf never walking (the same); runLastSQL's offset one further (the
// span ends a push late).
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
		// A PAGE ALREADY HOLDING an earlier repository's rows counts them:
		// one push more fills it.
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
