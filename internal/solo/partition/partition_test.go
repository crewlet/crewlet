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
// started internal/search, internal/store and internal/worklet last of 147;
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

// EVERY PACKAGE OF A HALF IS DEALT TO EXACTLY ONE SHARD, whatever the weights.
//
// Halves of 1 to 60 packages into every shard count up to the half's size,
// under no weights, equal ones, zeros, a skewed spread and one package that
// outweighs the rest together. No shard is left empty, and no shard carries
// more than an even split plus the longest package (the deal's bound).
func TestEveryPackageIsDealtToExactlyOneShard(t *testing.T) {
	t.Parallel()

	spreads := map[string]func(i int) float64{
		"no weights": nil,
		"all equal":  func(int) float64 { return 7 },
		"all zero":   func(int) float64 { return 0 },
		"some zero":  func(i int) float64 { return float64(i % 3 * (i * 13 % 97)) },
		"skewed":     func(i int) float64 { return float64((i*37)%101) * float64(i%5+1) },
		"one giant": func(i int) float64 {
			if i == 3 {
				return 1735
			}
			return float64(i%9 + 1)
		},
	}
	for name, spread := range spreads {
		for size := 1; size <= 60; size++ {
			half := make([]string, size)
			var measured map[string]float64
			if spread != nil {
				measured = map[string]float64{}
			}
			for i := range half {
				// Distinct for every i below 1000, and out of path order.
				half[i] = fmt.Sprintf("x/p%03d", i*7919%1000)
				if spread != nil {
					measured[half[i]] = spread(i)
				}
			}
			weights, _ := weigh(half, measured)
			var total, longest float64
			for _, p := range half {
				total += weights[p]
				longest = max(longest, weights[p])
			}
			for n := 1; n <= size; n++ {
				shards := deal(half, weights, n)
				if err := dealtOnce(half, shards); err != nil {
					t.Fatalf("%s, %d packages into %d shards: %v", name, size, n, err)
				}
				got := slices.Concat(shards...)
				slices.Sort(got)
				want := slices.Sorted(slices.Values(half))
				if !slices.Equal(got, want) {
					t.Fatalf("%s, %d packages into %d shards: the shards hold %v, the half %v", name, size, n, got, want)
				}
				for i, s := range shards {
					var load float64
					for _, p := range s {
						load += weights[p]
					}
					if bound := total/float64(n) + longest; load > bound+1e-9 {
						t.Errorf("%s, %d into %d: shard %d carries %v, over an even split plus the longest, %v",
							name, size, n, i+1, load, bound)
					}
				}
			}
		}
	}
}

// THE CHECK BEHIND EVERY DEAL REFUSES each way a deal can lose or double a
// package, so a regression in deal cannot print a short shard and pass.
func TestTheDealCheckRefusesALostADoubledOrAnEmptyShard(t *testing.T) {
	t.Parallel()

	half := []string{"x/a", "x/b", "x/c"}
	for name, c := range map[string]struct {
		shards [][]string
		says   string
	}{
		"a lost package":    {[][]string{{"x/a"}, {"x/b"}}, "x/c is in no shard"},
		"a doubled package": {[][]string{{"x/a", "x/b"}, {"x/b", "x/c"}}, "x/b is in shards 1 and 2"},
		"an empty shard":    {[][]string{{"x/a", "x/b", "x/c"}, {}}, "shard 2/2 is empty"},
		"a stranger":        {[][]string{{"x/a", "x/b"}, {"x/c", "x/z"}}, "the shards hold 4 packages, the half 3"},
	} {
		if err := dealtOnce(half, c.shards); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: dealtOnce = %v, want an error saying %q", name, err, c.says)
		}
	}
	if err := dealtOnce(half, [][]string{{"x/c"}, {"x/a", "x/b"}}); err != nil {
		t.Errorf("a whole deal was refused: %v", err)
	}
}

// THE LONGEST PACKAGES GET SHARDS OF THEIR OWN, and each shard runs longest
// first, so the package that sets the wall time starts at once with the least
// beside it.
func TestTheDealSpreadsTheLongestAndEachShardRunsLongestFirst(t *testing.T) {
	t.Parallel()

	half := []string{"x/api", "x/chatlet", "x/code", "x/engine", "x/host", "x/search", "x/tiny", "x/worklet"}
	measured := map[string]float64{
		"x/api": 133, "x/chatlet": 244, "x/code": 432, "x/engine": 1200,
		"x/host": 1735, "x/search": 393, "x/tiny": 1, "x/worklet": 337,
	}
	want := [][]string{
		{"x/host"},
		{"x/engine"},
		{"x/code", "x/chatlet", "x/api"},
		{"x/search", "x/worklet", "x/tiny"},
	}
	for range 100 {
		weights, _ := weigh(half, maps.Clone(measured))
		if got := deal(half, weights, 4); !slices.EqualFunc(got, want, slices.Equal) {
			t.Fatalf("deal = %v, want %v", got, want)
		}
	}
}

// WITHOUT WEIGHTS THE DEAL IS ROUND-ROBIN over go list's order: a split by
// package count, which is what a run with no measurement gets.
func TestWithoutWeightsTheDealIsRoundRobin(t *testing.T) {
	t.Parallel()

	half := []string{"x/z", "x/m", "x/a", "x/m/b", "x/q", "x/c", "x/d"}
	weights, _ := weigh(half, nil)
	want := [][]string{{"x/z", "x/m/b", "x/d"}, {"x/m", "x/q"}, {"x/a", "x/c"}}
	if got := deal(half, weights, 3); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("deal = %v, want %v", got, want)
	}
	if got := deal(half, weights, 1); !slices.EqualFunc(got, [][]string{half}, slices.Equal) {
		t.Errorf("one shard = %v, want the half as listed, %v", got, half)
	}
}

// -shard reads I/N with 1 <= I <= N, and nothing else.
func TestTheShardFlagReadsIOfN(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]shard{"": {1, 1}, "1/1": {1, 1}, "2/4": {2, 4}, "4/4": {4, 4}} {
		if got, err := parseShard(in); err != nil || got != want {
			t.Errorf("parseShard(%q) = %v, %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"0/4", "5/4", "4", "/4", "1/", "1/0", "-1/2", "a/b", "1/2/3", " 1/2", "1 /2"} {
		if got, err := parseShard(in); err == nil {
			t.Errorf("parseShard(%q) = %v, want a refusal", in, got)
		}
	}
}

// MERGE JOINS N RUNNERS' TIMINGS into one file the strict reader accepts, and
// refuses a package two runners timed or a file it would refuse as weights.
func TestMergeJoinsDisjointTimingsAndRefusesOverlap(t *testing.T) {
	t.Parallel()

	got, err := merge([]timings{
		{"shard-2.tsv", []byte("x/b\t2.000\nx/d\t0.000\n")},
		{"shard-1.tsv", []byte("x/a\t1.500\n\nx/c\t3.250\n")},
	})
	if err != nil {
		t.Fatalf("disjoint timings were refused: %v", err)
	}
	if want := "x/a\t1.500\nx/b\t2.000\nx/c\t3.250\nx/d\t0.000\n"; string(got) != want {
		t.Errorf("merged\n%q\nwant\n%q", got, want)
	}
	if _, err := readWeights(strings.NewReader(string(got)), "merged"); err != nil {
		t.Errorf("the merged file is not one the reader accepts: %v", err)
	}

	for name, c := range map[string]struct {
		inputs []timings
		says   string
	}{
		"a package two runners timed": {
			[]timings{{"one.tsv", []byte("x/a\t1\nx/b\t2\n")}, {"two.tsv", []byte("x/b\t2\n")}},
			"x/b is timed by both one.tsv and two.tsv",
		},
		"a line the reader refuses": {
			[]timings{{"one.tsv", []byte("x/a\t1\n")}, {"two.tsv", []byte("x/b 2\n")}},
			"two.tsv:1",
		},
	} {
		if _, err := merge(c.inputs); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: merge = %v, want an error saying %q", name, err, c.says)
		}
	}
}
