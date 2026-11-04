package textindex_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/textindex"
)

// FIND NAMES EVERY TERM AND THE FIRST, AS THE ANALYZER READS THE TEXT. A text
// holds a query's terms only as whole terms the analyzer makes of it — case
// folded, split on anything that is not a letter or a digit, a word cut where
// the analyzer cuts it — never as a piece of a longer word; it holds them all
// or it does not; and the first is the byte offset where the earliest of them
// begins, whichever the query named first. Over every case, what Find says
// agrees with Analyze: every term holds where Analyze counts every one, the
// term at the offset is one of them, and Analyze finds none before it.
//
// Mutation: find a term as a substring of the text folded to lower case, and
// a case goes red.
func TestFindNamesEveryTermAndTheFirst(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", textindex.MaxTermLength+6)
	for _, c := range []struct {
		name, text, query string
		first             int
		all               bool
	}{
		{"every term, the earliest first", "deploy the pipeline", "pipeline deploy", 0, true},
		{"a term inside a longer word is none", "redeployment done", "deploy", -1, false},
		{"case folded", "We DEPLOY now", "deploy", 3, true},
		{"split on what is not a letter or digit", "work_items-deploy", "items deploy", 5, true},
		{"one term of two", "deploy only", "deploy rollback", 0, false},
		{"bytes, not characters, before it", "über deploy", "deploy", len("über "), true},
		{"the earliest, whichever the query named first", "alpha beta gamma", "gamma beta", 6, true},
		{"a long word cut as the analyzer cuts it", "see " + long + " here", long, 4, true},
		{"a one-letter word is no term", "a b deploy", "a deploy", 4, true},
		{"a term written twice is found at its first", "ship it, then ship more", "ship", 0, true},
		{"nothing to find", "deploy", "", -1, false},
	} {
		terms := textindex.Terms(c.query)
		first, all := textindex.Find(c.text, terms)
		if first != c.first || all != c.all {
			t.Errorf("%s: Find(%q, %q) = (%d, %v), want (%d, %v)", c.name, c.text, terms, first, all,
				c.first, c.all)
		}
		analyzed := textindex.Analyze(c.text)
		every := len(terms) > 0 && !slices.ContainsFunc(terms, func(term string) bool { return analyzed[term] == 0 })
		if all != every {
			t.Errorf("%s: Find says every term is held: %v, and Analyze counts %v of %q", c.name, all,
				analyzed, terms)
		}
		if first < 0 {
			continue
		}
		if at := textindex.Terms(c.text[first:]); len(at) == 0 || !slices.Contains(terms, at[0]) {
			t.Errorf("%s: the text at %d reads %q, want one of %q there", c.name, first, at, terms)
		}
		before := textindex.Analyze(c.text[:first])
		if slices.ContainsFunc(terms, func(term string) bool { return before[term] > 0 }) {
			t.Errorf("%s: Analyze finds one of %q before %d: %v", c.name, terms, first, before)
		}
	}
}

// pieces is text handed over as FindIn reads it: each piece with the offset it
// begins at.
func pieces(text string, cuts ...int) func(yield func(int, string) bool) {
	return func(yield func(int, string) bool) {
		from := 0
		for _, cut := range append(cuts, len(text)) {
			if !yield(from, text[from:cut]) {
				return
			}
			from = cut
		}
	}
}

// FIND IN A TEXT'S PIECES READS THEM AS THE TEXT: every term is looked for in
// every piece, the first is an offset in the whole text however many pieces
// came before it, and a term is read within one piece — a word a cut parts is
// two pieces of a word, not the word — so a text cut where no term runs on is
// read exactly as Find reads it whole.
//
// Mutation: give the first as an offset in its own piece, or stop at the first
// piece, and a case goes red.
func TestFindInReadsATextsPiecesAsTheText(t *testing.T) {
	t.Parallel()
	text := "alpha [cut] beta gamma"
	for _, c := range []struct {
		name  string
		cuts  []int
		query string
		first int
		all   bool
	}{
		{"one piece is Find", nil, "gamma beta", 12, true},
		{"the first counted in the whole text", []int{6, 11}, "gamma beta", 12, true},
		{"every piece is read", []int{6, 11}, "alpha gamma", 0, true},
		{"a word a cut parts is not the word", []int{14}, "beta", -1, false},
	} {
		first, all := textindex.FindIn(pieces(text, c.cuts...), textindex.Terms(c.query))
		if first != c.first || all != c.all {
			t.Errorf("%s: FindIn = (%d, %v), want (%d, %v)", c.name, first, all, c.first, c.all)
		}
		if c.cuts == nil {
			if wf, wa := textindex.Find(text, textindex.Terms(c.query)); wf != first || wa != all {
				t.Errorf("%s: FindIn of one piece says (%d, %v) and Find (%d, %v)", c.name, first, all, wf, wa)
			}
		}
	}
}
