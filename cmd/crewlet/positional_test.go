package main

import (
	"flag"
	"testing"
)

// parsed is a flag set that has consumed args, so a case can be written the
// way an operator types it.
func parsed(t *testing.T, args ...string) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("space", "", "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("Parse(%v): %v", args, err)
	}
	return fs
}

// BOTH FORMS LAND THE SAME VALUE, and the count is the TOTAL.
//
// Go's flag package stops at the first non-flag token, so a command that read
// only the flags would silently discard `crewlet run /etc/crewlet.yaml -debug`
// and boot from its default path. Every command peels a leading positional
// before parsing, and the same value may arrive trailing instead — so the
// value can come from either side and the refusal has to count both.
func TestOnePositionalTakesTheValueFromEitherSide(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		leading string
		args    []string
		want    string
		given   int
	}{
		{"nothing at all", "", nil, "", 0},
		{"leading", "a.yaml", []string{"-space", "ENG"}, "a.yaml", 1},
		{"trailing", "", []string{"-space", "ENG", "a.yaml"}, "a.yaml", 1},
		// BOTH is two, which every caller refuses: they would have to
		// agree and nothing checks that they do.
		{"both", "a.yaml", []string{"-space", "ENG", "b.yaml"}, "a.yaml", 2},
		// AND THIS IS THE ONE THE OLD CODE MISCOUNTED. Two trailing
		// documents and no leading one is TWO. `len(tail)+1` said three,
		// in two of the three commands that print the number.
		{"two trailing", "", []string{"a.yaml", "b.yaml"}, "", 2},
		{"three trailing", "", []string{"a.yaml", "b.yaml", "c.yaml"}, "", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			value, given := onePositional(parsed(t, tc.args...), tc.leading)
			if given != tc.given {
				t.Errorf("given = %d, want %d", given, tc.given)
			}
			if value != tc.want {
				t.Errorf("value = %q, want %q", value, tc.want)
			}
		})
	}
}

// THE PAIR FILLS IN ORDER, whichever side each half arrived from.
func TestTwoPositionalsFillTheEmptySlotsInOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		first, second string
		args          []string
		wantA, wantB  string
		given         int
	}{
		{"both leading", "c.yaml", "./docs", []string{"-space", "ENG"}, "c.yaml", "./docs", 2},
		{"both trailing", "", "", []string{"-space", "ENG", "c.yaml", "./docs"}, "c.yaml", "./docs", 2},
		// The shape `crewlet confluence import c.yaml -space ENG ./docs`
		// arrives as: the flag package peels the leading one, the rest
		// lands in the tail.
		{"split", "c.yaml", "", []string{"-space", "ENG", "./docs"}, "c.yaml", "./docs", 2},
		{"only one", "", "", []string{"c.yaml"}, "c.yaml", "", 1},
		{"none", "", "", nil, "", "", 0},
		{"one too many", "", "", []string{"c.yaml", "./docs", "extra"}, "c.yaml", "./docs", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b, given := twoPositionals(parsed(t, tc.args...), tc.first, tc.second)
			if given != tc.given {
				t.Errorf("given = %d, want %d", given, tc.given)
			}
			if a != tc.wantA || b != tc.wantB {
				t.Errorf("= (%q, %q), want (%q, %q)", a, b, tc.wantA, tc.wantB)
			}
		})
	}
}
