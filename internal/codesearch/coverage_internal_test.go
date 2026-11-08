package codesearch

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
)

// A COVERAGE'S WORDS FIT ONE BUDGET: a coverage at its worst — every set
// holding thousands of repositories of the longest names, forty Gitlet nodes of
// the longest ids silent, each with a long reason full of characters JSON
// escapes — encodes in at most its shape (under 600 bytes), MaxCoverageText
// of names and reasons, and one entry of under 140 bytes for each silent
// node; every count stays whole, every set lists its FIRST names in order,
// and the reasons take at most half the budget, so names are listed too.
//
// Mutations: give every set its own cap rather than the one budget, and the
// coverage is many times past it, red; leave the reasons unbounded, and they
// take the whole budget, red.
func TestACoveragesWordsFitOneBudget(t *testing.T) {
	t.Parallel()
	const nodes, perSet = 40, 3000
	names := func(prefix string) RepoSet {
		var s RepoSet
		for i := range perSet {
			// 63 bytes, gitref.MaxRepoNameBytes; added out of order, as
			// replies arrive.
			add(&s, fmt.Sprintf("%s-%04d-%s", prefix, (i*7919)%perSet, strings.Repeat("n", 63-len(prefix)-6)))
		}
		return s
	}
	c := Coverage{Searched: math.MaxInt32, SkippedFiles: math.MaxInt32, Truncated: true}
	for i, s := range []*RepoSet{&c.NotIndexed, &c.Building, &c.NotPermitted, &c.GrepNotNamed, &c.Stale,
		&c.NoRef, &c.PastLimit} {
		*s = names(fmt.Sprintf("set%d", i))
	}
	for i := range nodes {
		c.NotAnswering = append(c.NotAnswering, NodeGap{Node: fmt.Sprintf("%02d%s", i, strings.Repeat("g", 62)),
			Why: strings.Repeat(`a "quoted" <reason> & more — `, 40), Repos: names(fmt.Sprintf("n%02d", i))})
	}
	want := c.NotIndexed.Count
	fitCoverage(&c)
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}

	// THE SHAPE AND ONE NODE'S ENTRY, at their longest and holding no word.
	shape, err := json.Marshal(Coverage{Searched: math.MaxInt, SkippedFiles: math.MaxInt, Truncated: true,
		NotIndexed: RepoSet{Count: math.MaxInt, Names: []string{}}, Building: RepoSet{Count: math.MaxInt,
			Names: []string{}}, NotPermitted: RepoSet{Count: math.MaxInt, Names: []string{}},
		GrepNotNamed: RepoSet{Count: math.MaxInt, Names: []string{}}, Stale: RepoSet{Count: math.MaxInt,
			Names: []string{}}, NoRef: RepoSet{Count: math.MaxInt, Names: []string{}},
		PastLimit: RepoSet{Count: math.MaxInt, Names: []string{}}, NotAnswering: []NodeGap{}})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := json.Marshal(NodeGap{Node: strings.Repeat("g", 64), Repos: RepoSet{Count: math.MaxInt,
		Names: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(shape) >= 600 || len(entry)+1 >= 140 {
		t.Errorf("the shape is %d bytes and a node's entry %d, want under the 600 and 140 MaxCoverageText states",
			len(shape), len(entry)+1)
	}
	if most := len(shape) + MaxCoverageText + nodes*(len(entry)+1); len(raw) > most {
		t.Errorf("the worst coverage encodes in %d bytes, past the %d its shape, its budget and %d nodes' "+
			"entries allow", len(raw), most, nodes)
	}

	if c.NotIndexed.Count != want || c.NotAnswering[0].Repos.Count != perSet {
		t.Errorf("counts %d and %d, want both whole at %d", c.NotIndexed.Count, c.NotAnswering[0].Repos.Count,
			perSet)
	}
	reasons, listed := 0, 0
	for _, g := range c.NotAnswering {
		reasons += len(g.Why)
		listed += len(g.Repos.Names)
	}
	for _, s := range []RepoSet{c.Building, c.Stale, c.NotPermitted, c.NoRef, c.PastLimit, c.NotIndexed,
		c.GrepNotNamed} {
		listed += len(s.Names)
		if !slices.IsSorted(s.Names) {
			t.Errorf("a set lists %v, not its first names in order", s.Names)
		}
	}
	if reasons > MaxCoverageText/2 || listed == 0 {
		t.Errorf("the reasons take %d bytes and %d names are listed, want at most %d and some", reasons, listed,
			MaxCoverageText/2)
	}
	first := c.NotAnswering[0].Repos.Names
	if len(first) == 0 || first[0] != fmt.Sprintf("n00-0000-%s", strings.Repeat("n", 63-3-6)) {
		t.Errorf("the first silent node lists %v, want its first name first", first)
	}
}
