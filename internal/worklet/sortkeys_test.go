package worklet

import (
	"slices"
	"testing"
	"time"
)

// The clock and zone this file's parses resolve against, its own: `wednesday`
// and `berlin` belong to the EXTERNAL suite, and `sortColumns` is unexported.
var (
	someWednesday = time.Date(2031, 4, 16, 14, 30, 0, 0, time.UTC)
	someZone      = time.UTC
)

// THE TWO HALVES OF A SORT KEY AGREE, and neither fails loudly alone.
//
// A key [sortColumns] or [derivedOrders] holds and [sortKeys] refuses is
// UNREACHABLE. A key [sortKeys] admits and neither compiles is worse:
// [sortTerms] silently drops the term and the answer comes back in the DEFAULT
// order, the failure [checkKeys] prevents for filters. A key in both would be
// compiled one way and documented the other.
func TestEverySortKeyCompilesAndEveryColumnIsReachable(t *testing.T) {
	t.Parallel()
	for _, key := range sortKeys {
		_, column := sortColumns[key]
		_, derived := derivedOrders[key]
		switch {
		case !column && !derived:
			t.Errorf("sort=%s is admitted by the parser and compiles to no "+
				"column — the term is dropped and the answer comes back in "+
				"the default order with nothing saying so", key)
		case column && derived:
			t.Errorf("sort=%s is both a column and a derived order", key)
		}
	}
	for column := range sortColumns {
		if !slices.Contains(sortKeys, column) {
			t.Errorf("sortColumns has %q and the parser refuses it — the "+
				"entry is unreachable", column)
		}
	}
	for key := range derivedOrders {
		if !slices.Contains(sortKeys, key) {
			t.Errorf("derivedOrders has %q and the parser refuses it, so the "+
				"entry is unreachable", key)
		}
	}
}

// AND A SORT KEY IS A PARAMETER VALUE, never read as a NAME: `sort=start`
// and `start=2026-01-01` are a column and a date filter, and `start` is both.
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
