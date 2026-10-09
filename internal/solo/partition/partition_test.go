package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// The partition rule, over synthetic go list records.
//
// Pure, so the cases that matter can be stated directly rather than reached by
// standing up a module and shelling out to the toolchain. The XTestImports
// case is the one with a real package behind it: internal/node and
// internal/statelog declare their suites as `package node_test` /
// `package statelog_test`, so their marker import lands in XTestImports. A
// rule reading only TestImports would call both of them ordinary and put two
// multi-member brokers back in the shared runner.
func TestTheMarkerIsReadFromBothTestImportLists(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		pkg  pkg
		solo bool
	}{
		{
			name: "an internal test suite declares it",
			pkg:  pkg{ImportPath: "x/e2e", TestImports: []string{"testing", marker}},
			solo: true,
		},
		{
			name: "an external test suite declares it",
			pkg:  pkg{ImportPath: "x/node", XTestImports: []string{"testing", marker}},
			solo: true,
		},
		{
			name: "a package with both lists declares it in either",
			pkg: pkg{
				ImportPath:   "x/statelog",
				TestImports:  []string{"testing"},
				XTestImports: []string{marker},
			},
			solo: true,
		},
		{
			name: "an ordinary package does not",
			pkg:  pkg{ImportPath: "x/config", TestImports: []string{"testing", "x/other"}},
			solo: false,
		},
		{
			name: "a package with no tests at all does not",
			pkg:  pkg{ImportPath: "x/version"},
			solo: false,
		},
		{
			// The marker is a TEST dependency. A non-test import of it would
			// pull `testing` into a shipped binary, which roster_test.go
			// refuses outright — so the partition must not quietly honour one.
			name: "a non-test import is not a declaration",
			pkg:  pkg{ImportPath: "x/engine", TestImports: []string{"testing"}},
			solo: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := declaresSolo(c.pkg); got != c.solo {
				t.Errorf("declaresSolo(%q) = %v, want %v", c.pkg.ImportPath, got, c.solo)
			}
		})
	}
}

// Every listed package lands in exactly one half.
//
// The guard main() makes on this is the one the shell filter could not: a
// package that fell out of BOTH sets is invisible to whoever runs either half,
// because each sees a plausible list and a zero exit.
func TestEveryPackageLandsInExactlyOneHalf(t *testing.T) {
	t.Parallel()

	pkgs := []pkg{
		{ImportPath: "x/a"},
		{ImportPath: "x/heavy", XTestImports: []string{marker}},
		{ImportPath: "x/b", TestImports: []string{"testing"}},
		{ImportPath: "x/heavier", TestImports: []string{marker}},
		{ImportPath: "x/c"},
	}

	parallel, solo := partition(pkgs)

	if want := []string{"x/a", "x/b", "x/c"}; !slices.Equal(parallel, want) {
		t.Errorf("parallel = %v, want %v", parallel, want)
	}
	if want := []string{"x/heavy", "x/heavier"}; !slices.Equal(solo, want) {
		t.Errorf("solo = %v, want %v", solo, want)
	}
	if len(parallel)+len(solo) != len(pkgs) {
		t.Errorf("%d + %d != %d listed", len(parallel), len(solo), len(pkgs))
	}
	for _, p := range parallel {
		if slices.Contains(solo, p) {
			t.Errorf("%s is in both halves", p)
		}
	}
}

// WITHOUT WEIGHTS, go list's order is preserved, so the lists a build prints
// are stable.
//
// Not cosmetic: the Makefile expands these into a command line, and a set that
// reordered between runs would make every `make test` invocation a different
// command for no reason anybody could see in a diff. So the order is held from
// the listing itself, through partition, weigh and order, to what is printed.
// The listing here is deliberately not sorted, with the two halves
// interleaved, so a step that sorted by path would fail rather than agree by
// accident.
func TestWithoutWeightsTheListedOrderIsPreserved(t *testing.T) {
	t.Parallel()

	pkgs := []pkg{
		{ImportPath: "x/z"},
		{ImportPath: "x/y", XTestImports: []string{marker}},
		{ImportPath: "x/m"},
		{ImportPath: "x/b", TestImports: []string{marker}},
		{ImportPath: "x/a"},
		{ImportPath: "x/m/b"},
	}
	parallel, solo := partition(pkgs)

	for _, c := range []struct {
		name       string
		half, want []string
	}{
		{"the parallel half", parallel, []string{"x/z", "x/m", "x/a", "x/m/b"}},
		{"the solo half", solo, []string{"x/y", "x/b"}},
	} {
		if !slices.Equal(c.half, c.want) {
			t.Errorf("%s = %v, want %v (go list order)", c.name, c.half, c.want)
		}
		weights, unmeasured := weigh(c.half, nil)
		if unmeasured != len(c.half) {
			t.Errorf("%s: unmeasured = %d, want all %d — no weights were given",
				c.name, unmeasured, len(c.half))
		}
		if got := order(c.half, weights); !slices.Equal(got, c.want) {
			t.Errorf("%s: order = %v, want %v (go list order)", c.name, got, c.want)
		}
	}
}

// WITH WEIGHTS, THE LONGEST STARTS FIRST.
//
// `go test` starts package runs in command-line order, so the first argument
// is the first binary started, and the alphabetical order this replaced
// started internal/search, internal/store and internal/tracker last of 147 —
// the run then ended on those three with the rest of the runner idle. Ties
// keep the listed order, so the answer is a function of the list and the file
// alone; it is asked a hundred times, from a map rebuilt each time, so an
// order that leaked map iteration would not survive.
func TestWithWeightsTheLongestStartsFirst(t *testing.T) {
	t.Parallel()

	half := []string{"x/api", "x/engine", "x/gate", "x/search", "x/tiny", "x/tracker"}
	measured := map[string]float64{
		"x/api":     27,
		"x/engine":  366,
		"x/search":  472,
		"x/tiny":    1,
		"x/tracker": 412,
		"x/gate":    27, // ties x/api, which go list puts first
	}
	want := []string{"x/search", "x/tracker", "x/engine", "x/api", "x/gate", "x/tiny"}

	for range 100 {
		weights, unmeasured := weigh(half, maps.Clone(measured))
		if unmeasured != 0 {
			t.Fatalf("unmeasured = %d, want 0", unmeasured)
		}
		if got := order(half, weights); !slices.Equal(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}

	// TIES KEEP THE LISTED ORDER IN A HALF OF ANY SIZE. The six above cannot
	// tell a stable sort from one that is not: below thirteen elements Go's
	// sort is an insertion sort, stable by accident, and the parallel half is
	// well over a hundred packages long. So forty packages, listed out of path
	// order, at three weights between them: each weight's packages must come
	// out in the order they were listed. Three weights rather than one,
	// because a pdqsort leaves a run that is ALL equal where it found it, so
	// forty ties at one weight cannot tell either.
	var many []string
	tiers := map[string]float64{}
	byTier := map[float64][]string{}
	for i := range 40 {
		p := fmt.Sprintf("x/p%02d", (i*17)%40)
		many = append(many, p)
		tiers[p] = float64(i % 3)
		byTier[tiers[p]] = append(byTier[tiers[p]], p)
	}
	want = slices.Concat(byTier[2], byTier[1], byTier[0])
	weights, _ := weigh(many, tiers)
	if got := order(many, weights); !slices.Equal(got, want) {
		t.Errorf("forty packages at three weights came out as\n%v\nwant each weight's packages in the listed order\n%v",
			got, want)
	}
}

// A PACKAGE THE MEASUREMENT DOES NOT NAME WEIGHS THE HALF'S MEDIAN.
//
// A package new since the measured run is neither the longest nor the
// shortest thing anybody knows of, so it goes mid-queue. The median is of
// THIS half: a file may time the other half's packages too, and internal/e2e's
// seconds must not decide where a new parallel package starts.
func TestAnUnmeasuredPackageWeighsTheMedianOfItsOwnHalf(t *testing.T) {
	t.Parallel()

	half := []string{"x/a", "x/b", "x/c", "x/new"}
	measured := map[string]float64{
		"x/a": 10, "x/b": 20, "x/c": 30,
		"x/e2e": 500, // the other half's
	}
	weights, unmeasured := weigh(half, measured)
	if unmeasured != 1 {
		t.Errorf("unmeasured = %d, want 1", unmeasured)
	}
	if weights["x/new"] != 20 {
		t.Errorf("the unmeasured package weighs %v, want 20 — the median of this half's 10, 20, 30",
			weights["x/new"])
	}
	if _, leaked := weights["x/e2e"]; leaked {
		t.Errorf("a package of the other half was weighed into this one: %v", weights)
	}

	// An even count takes the mean of the middle two.
	weights, _ = weigh([]string{"x/a", "x/b", "x/new"}, map[string]float64{"x/a": 10, "x/b": 20})
	if weights["x/new"] != 15 {
		t.Errorf("with two measured, the unmeasured package weighs %v, want 15", weights["x/new"])
	}
}

// A WEIGHTS FILE IS READ STRICTLY, because its one producer is
// internal/skipgate and a line it did not write is a bug upstream that would
// otherwise surface as a mysteriously ordered run.
func TestAWeightsFileIsReadStrictly(t *testing.T) {
	t.Parallel()

	got, err := readWeights(strings.NewReader("x/a\t12.5\n\nx/b\t0\nx/c\t3\n"), "w.tsv")
	if err != nil {
		t.Fatalf("a well-formed file was refused: %v", err)
	}
	if want := map[string]float64{"x/a": 12.5, "x/b": 0, "x/c": 3}; !maps.Equal(got, want) {
		t.Errorf("read %v, want %v", got, want)
	}

	for name, c := range map[string]struct{ text, says string }{
		"no tab":            {"x/a 12\n", "w.tsv:1"},
		"no path":           {"\t12\n", "w.tsv:1"},
		"a path with space": {"x/a b\t12\n", "w.tsv:1"},
		"not a number":      {"x/a\t12\nx/b\tslow\n", "w.tsv:2"},
		"two tabs":          {"x/a\t1\t2\n", "w.tsv:1"},
		"negative":          {"x/a\t-1\n", "non-negative"},
		"NaN":               {"x/a\tNaN\n", "finite"},
		"infinite":          {"x/a\t+Inf\n", "finite"},
		"named twice":       {"x/a\t1\nx/b\t2\nx/a\t3\n", "named twice"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := readWeights(strings.NewReader(c.text), "w.tsv"); err == nil || !strings.Contains(err.Error(), c.says) {
				t.Errorf("readWeights(%q) = %v, want an error saying %q", c.text, err, c.says)
			}
		})
	}
}
