package store_test

import (
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// THE CHUNK SIZE COMES FROM THE PROBED LIMIT, not from a constant.
//
// A fixed 999 is wrong in both directions: on an engine with the modern
// default it is 33× more round trips than necessary, and on one with a lower
// limit it is a statement the engine refuses — and the refusal arrives at the
// end of a bulk apply, on a node that was fine yesterday.
func TestMultiRowChunksRespectTheProbedLimit(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		variables int
		columns   int
		want      int
	}{
		"the modern default":    {32766, 8, 1000},
		"a narrow row":          {32766, 2, 1000},
		"a wide row":            {32766, 64, 511},
		"the historical limit":  {999, 8, 124},
		"a row at the limit":    {999, 999, 1},
		"a row past the limit":  {999, 1200, 1},
		"a row that binds none": {32766, 0, 1000},
		"columns below zero":    {32766, -3, 1000},
	} {
		t.Run(name, func(t *testing.T) {
			if got := store.RowsPerInsert(tc.variables, tc.columns); got != tc.want {
				t.Errorf("RowsPerInsert(%d, %d) = %d, want %d",
					tc.variables, tc.columns, got, tc.want)
			}
		})
	}
}

// AND THE PROBED LIMIT IS A REAL NUMBER on the live driver — which is what
// makes the arithmetic above worth doing. A capability probe that answered 0
// would chunk every insert to one row and the drain figure would be the
// unprepared one with extra steps.
func TestTheProbedVariableLimitBoundsARealInsert(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "vars.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	limit := db.Caps().MaxVariables
	if limit < 999 {
		t.Fatalf("the probe reports %d bind variables, which is below every "+
			"engine this driver has shipped", limit)
	}
	if got := store.RowsPerInsert(limit, 4); got < 1 {
		t.Errorf("a four-column row chunks to %d rows", got)
	}
}

// CHUNKS COVERS EVERY ROW EXACTLY ONCE, which is the property a caller writing
// child rows depends on: a gap is a row silently not written, and an overlap
// is a duplicate-key failure at the end of a long apply.
func TestChunksCoverEveryRowOnce(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 999, 1000, 1001, 4000} {
		var seen int
		last := 0
		for start, end := range store.Chunks(n, 32766, 8) {
			if start != last {
				t.Fatalf("n=%d: chunk starts at %d, want %d", n, start, last)
			}
			if end <= start {
				t.Fatalf("n=%d: empty chunk [%d,%d)", n, start, end)
			}
			seen += end - start
			last = end
		}
		if seen != n {
			t.Errorf("n=%d: chunks covered %d rows", n, seen)
		}
	}
}
