package codeindex

import (
	"bytes"
	"math/rand/v2"
	"regexp"
	"regexp/syntax"
	"testing"
)

// gitAdmits reports whether git grep -E, given pattern — Grep's alternation
// of escaped strings — and told -i where fold is set, answers line, read as
// git reads it under gitproc's LC_ALL=C: Go's POSIX ERE over the line, with
// ASCII letters folded alone under -i — git's -i lowers no rune outside
// ASCII, which is what a clause must be read past for.
func gitAdmits(t *testing.T, line []byte, pattern string, fold bool) bool {
	t.Helper()
	if fold {
		pattern, line = string(asciiLower([]byte(pattern))), asciiLower(line)
	}
	re, err := regexp.CompilePOSIX(pattern)
	if err != nil {
		t.Fatalf("Grep's expression %q is no extended regular expression: %v", pattern, err)
	}
	return re.Match(line)
}

// GIT GREP'S STRINGS MISS NO LINE THE EXPRESSION MATCHES (M6-D13).
//
// The grep path's promise beside the index's: git grep -E, over the
// alternation of the strings of the clause Grep chose, answers a superset of the lines the expression
// matches, which the search then confirms with Go's regexp — so a repository
// read with git grep answers the lines the index would. Held first on what
// git's ASCII-only -i cannot see — the long s and the Kelvin sign, which RE2
// folds onto s and k, and İ, which a folded literal's lowering turns into i,
// all of which Grep looks for as strings of their own — and on a
// clause folding a letter outside ASCII, which git cannot read exactly and is
// never chosen; and on strings holding the expression syntax, which the
// alternation escapes; then over the generated patterns and blobs of the plan's own
// property (TestAPlanNeverRejectsAMatchGoRegexpFinds), every line Go's regexp
// matches required among git's.
//
// Mutation: drop asciiFolds from a folded clause's strings (Grep) — the long
// s's status is missed; take no rune whose lowering is ASCII (asciiFolds'
// case ranges) — İSTANBUL is missed; choose a clause whatever its folded literals (Grep's
// exact) — (?i)école is answered with strings git's -i misses ÉCOLE by;
// escape nothing (Grep's ereMeta) — a+b.c is read as an expression and the
// line holding it is missed.
func TestGitGrepsStringsMissNoLineTheExpressionMatches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern, line string
		ok            bool
	}{
		{`(?i)status`, "ſtatus: ok", true},
		{`(?i)STATUS`, "Status", true},
		{`(?i)kelvin`, "Kelvin", true},
		{`(?i)İstanbul`, "İSTANBUL", true},
		{`(?i)école`, "ÉCOLE", false},
		{`École`, "l'École", true},
		{`(?i)école|status`, "status", false},
		{`(?i)école.*status`, "ÉCOLE status", true},
		{`a\+b\.c \(x\|y\)`, "z = a+b.c (x|y)", true},
		{`\[\^\$\{2\}\\`, "[^${2}\\", true},
	} {
		re := regexp.MustCompile(tc.pattern)
		plan, err := NewPlan(re)
		if err != nil {
			t.Fatalf("NewPlan(%s): %v", tc.pattern, err)
		}
		if !re.MatchString(tc.line) {
			t.Fatalf("the case is wrong: %s does not match %q", tc.pattern, tc.line)
		}
		grep, fold, ok := plan.Grep()
		if ok != tc.ok {
			t.Errorf("Grep of %s answers (%q, %t, %t), want ok %t", tc.pattern, grep, fold, ok, tc.ok)
		}
		if ok && !gitAdmits(t, []byte(tc.line), grep, fold) {
			t.Errorf("git grep -E (-i %t) over %q misses %q, which %s matches", fold, grep, tc.line,
				tc.pattern)
		}
	}

	rng := rand.New(rand.NewPCG(20261101, 5))
	var planned, chosen, lines int
	for range 1500 {
		pattern := vocabulary.Pattern(rng, 2)
		re, err := regexp.Compile(pattern)
		if err != nil {
			t.Fatalf("the generator wrote %q, which does not compile: %v", pattern, err)
		}
		plan, err := NewPlan(re)
		if err != nil {
			continue
		}
		planned++
		grep, fold, ok := plan.Grep()
		if !ok {
			continue
		}
		chosen++
		parsed, err := syntax.Parse(pattern, syntax.Perl)
		if err != nil {
			t.Fatalf("parse %q: %v", pattern, err)
		}
		for range 8 {
			for line := range bytes.SplitSeq(blobOf(rng, parsed), []byte{'\n'}) {
				if !re.Match(line) {
					continue
				}
				lines++
				if !gitAdmits(t, line, grep, fold) {
					t.Fatalf("git grep -E (-i %t) over %q misses %q, which %s matches", fold, grep, line,
						pattern)
				}
			}
		}
	}
	// THE PROPERTY WAS MET: most plans give git strings, and their lines
	// were matched — a Grep answering ok false always would pass the loop.
	if chosen < planned/2 || lines < chosen {
		t.Fatalf("%d of %d plans gave git strings, over %d matched lines: the property was hardly exercised",
			chosen, planned, lines)
	}
	t.Logf("%d patterns planned, %d read by git grep's strings; %d matched lines all admitted", planned, chosen,
		lines)
}
