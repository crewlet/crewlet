package codesearch

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// A MERGE HOLDS THE FIRST LIMIT LINES, EACH ONCE: folded answer by answer, in
// any order, it never holds more than its limit, and ends holding exactly
// what every answer sorted together, each line once, and cut at the limit
// would be — cut set where a line was dropped, and not where only a line
// named twice was.
//
// Mutation: fold every line in and cut only when read, and the merge holds
// past its limit, red.
func TestAMergeHoldsTheFirstLimitLinesOnce(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(7, 11))
	var answers [][]Hit
	var all []Hit
	for r := range 40 {
		var hits []Hit
		for l := range 12 {
			hits = append(hits, Hit{Repo: fmt.Sprintf("r%02d", rng.IntN(40)), SearchHit: gitlet.SearchHit{
				Path: fmt.Sprintf("p%d", rng.IntN(5)), Line: l + r}})
		}
		answers = append(answers, hits)
		all = append(all, hits...)
	}
	slices.SortFunc(all, CompareHits)
	all = slices.CompactFunc(all, func(x, y Hit) bool { return CompareHits(x, y) == 0 })
	for _, limit := range []int{1, 7, 50, len(all), len(all) + 3} {
		m := newMerge(limit)
		for _, i := range rng.Perm(len(answers)) {
			m.fold(slices.Clone(answers[i]))
			if held, _ := m.lines(); len(held) > limit {
				t.Fatalf("limit %d: the merge holds %d lines", limit, len(held))
			}
		}
		held, cut := m.lines()
		want := all[:min(limit, len(all))]
		if !reflect.DeepEqual(held, want) {
			t.Fatalf("limit %d: held %v, want %v", limit, held, want)
		}
		if cut != (limit < len(all)) {
			t.Errorf("limit %d of %d distinct lines: cut %t", limit, len(all), cut)
		}
	}
	m := newMerge(2)
	one := Hit{Repo: "a", SearchHit: gitlet.SearchHit{Path: "p", Line: 1}}
	two := Hit{Repo: "a", SearchHit: gitlet.SearchHit{Path: "p", Line: 2}}
	m.fold([]Hit{one, two})
	m.fold([]Hit{two})
	if _, cut := m.lines(); cut {
		t.Error("a line answered twice was read as one cut")
	}
}
