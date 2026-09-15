package tracker

import (
	"slices"
	"testing"
	"time"
)

// The clock and zone this file's parses resolve against. Its own, because
// `wednesday` and `berlin` belong to the EXTERNAL suite and this file is
// inside the package — `sortColumns` is unexported.
var (
	someWednesday = time.Date(2031, 4, 16, 14, 30, 0, 0, time.UTC)
	someZone      = time.UTC
)

// THE TWO HALVES OF A SORT KEY AGREE, and neither fails loudly alone.
//
// A key [sortColumns] holds and [sortKeys] refuses is UNREACHABLE — dead code
// carrying a comment about behaviour implemented somewhere else, which is what
// `removed` was. A key [sortKeys] admits and [sortColumns] lacks is worse: the
// parser accepts it, [sortTerms] silently drops the term, and the answer comes
// back in the DEFAULT order with nothing saying the caller's own ordering was
// ignored — the same failure shape [checkKeys] exists to prevent for filters.
func TestEverySortKeyCompilesAndEveryColumnIsReachable(t *testing.T) {
	t.Parallel()
	for _, key := range sortKeys {
		if _, known := sortColumns[key]; !known {
			t.Errorf("sort=%s is admitted by the parser and compiles to no "+
				"column — the term is dropped and the answer comes back in "+
				"the default order with nothing saying so", key)
		}
	}
	for column := range sortColumns {
		if !slices.Contains(sortKeys, column) {
			t.Errorf("sortColumns has %q and the parser refuses it — the "+
				"entry is unreachable", column)
		}
	}
}

// AND A SORT KEY IS A PARAMETER VALUE, so it may never be a parameter NAME's
// spelling by accident: `sort=start` and `start=2026-01-01` are a column and a
// date filter, and reading either as the other answers a question nobody
// asked. This is the pair that made it worth stating — `start` is both.
func TestASortKeyAndAFilterMayShareAName(t *testing.T) {
	t.Parallel()
	if !slices.Contains(sortKeys, "start") || !slices.Contains(QueryKeys, "start") {
		t.Fatal("the premise moved: `start` is meant to be both an ordering " +
			"and a filter")
	}
	q, err := ParseQuery(MapParams(map[string]any{
		"container": "project:ENG", "sort": "start", "start": "gte:today",
	}), someWednesday, someZone)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if len(q.Sort) != 1 || q.Sort[0].Key != "start" {
		t.Errorf("the order is %+v, want the one column asked for", q.Sort)
	}
	if _, filtered := q.Dates["start"]; !filtered {
		t.Error("the date filter was not read at all — a sort key sharing " +
			"its name swallowed it")
	}
}
