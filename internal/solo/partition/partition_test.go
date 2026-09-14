package main

import (
	"slices"
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

// go list's order is preserved, so the two lists a build prints are stable.
//
// Not cosmetic: the Makefile expands these into a command line, and a set that
// reordered between runs would make every `make test` invocation a different
// command for no reason anybody could see in a diff.
func TestTheListedOrderIsPreserved(t *testing.T) {
	t.Parallel()

	pkgs := []pkg{{ImportPath: "x/z"}, {ImportPath: "x/m"}, {ImportPath: "x/a"}}
	parallel, solo := partition(pkgs)

	if want := []string{"x/z", "x/m", "x/a"}; !slices.Equal(parallel, want) {
		t.Errorf("parallel = %v, want %v (go list order)", parallel, want)
	}
	if len(solo) != 0 {
		t.Errorf("solo = %v, want empty", solo)
	}
}
