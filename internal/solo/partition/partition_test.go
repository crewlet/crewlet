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
// command for no reason anybody could see in a diff. The whole half as one
// shard — the Makefile's default, `-shard 1/1` — is the same list. The
// listed order here is deliberately not sorted, so a rule that sorted by
// path would fail rather than agree by accident.
func TestWithoutWeightsTheListedOrderIsPreserved(t *testing.T) {
	t.Parallel()

	half := []string{"x/z", "x/m", "x/a", "x/m/b"}
	weights, unmeasured := weigh(half, nil)
	if unmeasured != len(half) {
		t.Errorf("unmeasured = %d, want all %d — no weights were given", unmeasured, len(half))
	}

	if got := order(half, weights); !slices.Equal(got, half) {
		t.Errorf("order = %v, want %v (go list order)", got, half)
	}
	if got := shard(order(half, weights), weights, 1); len(got) != 1 || !slices.Equal(got[0], half) {
		t.Errorf("shard 1/1 = %v, want the whole half in go list order %v", got, half)
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
}

// A PACKAGE THE MEASUREMENT DOES NOT NAME WEIGHS THE HALF'S MEDIAN.
//
// A package new since the measured run is neither the longest nor the
// shortest thing anybody knows of, so it goes mid-queue. The median is of
// THIS half: the file carries both halves, and internal/e2e's five hundred
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

// EVERY SHARDING COVERS THE HALF EXACTLY ONCE, whatever the weights say.
//
// This is the property the whole design rests on: weights choose WHICH runner
// a package goes to, never WHETHER it runs. So every shape a weight can take
// is offered — absent, measured, missing, zero, equal, enormous, and naming
// packages that no longer exist — at every shard count the half admits, and
// each result must deal every package to exactly one non-empty shard, in
// longest-first order within it.
func TestEveryShardingCoversTheHalfExactlyOnce(t *testing.T) {
	t.Parallel()

	half := []string{"x/a", "x/b", "x/c", "x/d", "x/e", "x/f", "x/g"}
	cases := map[string]map[string]float64{
		"no weights":    nil,
		"measured":      {"x/a": 5, "x/b": 400, "x/c": 30, "x/d": 30, "x/e": 1, "x/f": 120, "x/g": 7},
		"some missing":  {"x/b": 400, "x/f": 120},
		"all zero":      {"x/a": 0, "x/b": 0, "x/c": 0, "x/d": 0, "x/e": 0, "x/f": 0, "x/g": 0},
		"all equal":     {"x/a": 9, "x/b": 9, "x/c": 9, "x/d": 9, "x/e": 9, "x/f": 9, "x/g": 9},
		"one dominates": {"x/a": 1e9, "x/b": 1, "x/c": 1, "x/d": 1, "x/e": 1, "x/f": 1, "x/g": 1},
		"zeros and one": {"x/a": 0, "x/b": 0, "x/c": 50, "x/d": 0, "x/e": 0, "x/f": 0, "x/g": 0},
		"stale names":   {"x/gone": 900, "x/renamed": 300, "x/c": 40},
	}

	for name, measured := range cases {
		for n := 1; n <= len(half); n++ {
			t.Run(fmt.Sprintf("%s/%d", name, n), func(t *testing.T) {
				t.Parallel()
				weights, _ := weigh(half, measured)
				shards := shard(order(half, weights), weights, n)
				if len(shards) != n {
					t.Fatalf("%d shards, want %d", len(shards), n)
				}
				if err := cover(half, shards); err != nil {
					t.Fatalf("the shards do not cover the half: %v (%v)", err, shards)
				}
				for i, s := range shards {
					for j := 1; j < len(s); j++ {
						if weights[s[j-1]] < weights[s[j]] {
							t.Errorf("shard %d/%d starts %s (%v) before the longer %s (%v): %v",
								i+1, n, s[j-1], weights[s[j-1]], s[j], weights[s[j]], s)
						}
					}
				}
			})
		}
	}
}

// THE SHARDS BALANCE BY MEASURED TIME, which is what makes a second runner
// worth having.
//
// The solo half's own measured shape (CI seconds on main): internal/e2e alone
// is longer than the other six together, so two shards are e2e and everything
// else — and a third would buy nothing, since e2e is the floor.
func TestTheShardsBalanceByMeasuredTime(t *testing.T) {
	t.Parallel()

	half := []string{"x/kv", "x/e2e", "x/eventfantest", "x/node", "x/jetstreamtest", "x/statelog", "x/usage"}
	measured := map[string]float64{
		"x/e2e": 427, "x/kv": 124, "x/statelog": 95, "x/node": 35,
		"x/usage": 25, "x/jetstreamtest": 16, "x/eventfantest": 5,
	}
	weights, _ := weigh(half, measured)
	got := shard(order(half, weights), weights, 2)
	want := [][]string{
		{"x/e2e"},
		{"x/kv", "x/statelog", "x/node", "x/usage", "x/jetstreamtest", "x/eventfantest"},
	}
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("shards = %v, want %v", got, want)
	}

	// Equal weights split by count, dealt round in the listed order.
	listed := []string{"x/a", "x/b", "x/c", "x/d", "x/e"}
	weights, _ = weigh(listed, nil)
	got = shard(order(listed, weights), weights, 2)
	want = [][]string{{"x/a", "x/c", "x/e"}, {"x/b", "x/d"}}
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("unweighted shards = %v, want %v", got, want)
	}
}

// THE COVER CHECK FAILS ON EVERY WAY A SHARDING CAN BE WRONG.
//
// main runs it before printing anything, so it is the guard between a bug in
// shard and a CI run that quietly skipped a package on both runners. A guard
// is only worth its failure cases.
func TestTheCoverCheckRefusesEveryBadSharding(t *testing.T) {
	t.Parallel()

	half := []string{"x/a", "x/b", "x/c"}
	for name, c := range map[string]struct {
		shards [][]string
		says   string
	}{
		"a package in no shard":     {[][]string{{"x/a"}, {"x/b"}}, "x/c is in no shard"},
		"a package in two shards":   {[][]string{{"x/a", "x/b"}, {"x/b", "x/c"}}, "x/b is in shard 1/2 and shard 2/2"},
		"a package twice in one":    {[][]string{{"x/a", "x/a", "x/b", "x/c"}}, "x/a is in shard 1/1 and shard 1/1"},
		"a package not in the half": {[][]string{{"x/a", "x/b"}, {"x/c", "x/z"}}, "x/z is in a shard but not in the half"},
		"an empty shard":            {[][]string{{"x/a", "x/b", "x/c"}, {}}, "shard 2/2 is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := cover(half, c.shards)
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Errorf("cover(%v) = %v, want an error saying %q", c.shards, err, c.says)
			}
		})
	}
	if err := cover(half, [][]string{{"x/c"}, {"x/a", "x/b"}}); err != nil {
		t.Errorf("an exact cover was refused: %v", err)
	}
}

// A WEIGHTS FILE IS READ STRICTLY, because its one producer is
// internal/skipgate and a line it did not write is a bug upstream that would
// otherwise surface as a mysteriously unbalanced run.
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

// A SHARD SPEC IS I/N WITH 1 ≤ I ≤ N, and nothing else is guessed at.
func TestAShardSpecIsParsedStrictly(t *testing.T) {
	t.Parallel()

	for spec, want := range map[string][2]int{"1/1": {1, 1}, "1/2": {1, 2}, "2/2": {2, 2}, "3/7": {3, 7}} {
		i, n, err := parseShard(spec)
		if err != nil || i != want[0] || n != want[1] {
			t.Errorf("parseShard(%q) = %d, %d, %v; want %d, %d", spec, i, n, err, want[0], want[1])
		}
	}
	for _, spec := range []string{"", "1", "0/2", "3/2", "1/0", "-1/2", "a/2", "1/b", "1/2/3", "1 / 2"} {
		if _, _, err := parseShard(spec); err == nil {
			t.Errorf("parseShard(%q) was accepted", spec)
		}
	}
}
