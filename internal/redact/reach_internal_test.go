package redact

import (
	"strings"
	"testing"
)

// A RULE'S SEARCH ENDS WHATEVER ITS BOUND ANSWERS ([rule.bound]): it goes on
// from past where a match it refused began, even where the bound names that
// place, or one before it, to go on from. Taken as named, the JSON Web
// Token's search went on from where it began whenever its bound and its
// answer of where to go on disagreed by a byte, and found the same match for
// ever. So a bound that refuses both matches in `abab` and names the text's
// start is asked twice, once a match, and nothing is found.
//
// Mutation: go on from where the bound names alone, and the bound is asked
// until it gives in.
func TestARulesSearchEndsWhateverItsBoundAnswers(t *testing.T) {
	t.Parallel()
	asked := 0
	// Asked more often than any search of the text needs, it admits the
	// match, so a search that never moved on still ends, its count showing
	// it.
	refusing := token(`ab`, "x").bounded(func(string, int, int) (bool, int) {
		asked++
		return asked > 64, 0
	})
	if start, end, found := refusing.find("abab", 0); found || asked != 2 {
		t.Errorf("the search found %v (%d to %d) asking its bound %d times, want nothing found asking it "+
			"twice: it went on from where the bound named, the match's own start", found, start, end, asked)
	}
}

// A JSON WEB TOKEN'S HEADER OF MANY OPENINGS COSTS WHAT ITS BYTES COST: a
// token whose header and claims run past [MaxReach] is matched from the first
// `eyJ` from which they fit ([jwtWithin]), and finding it reads the header
// again once, not once for every `eyJ` in it. Looked for from the place after
// each match that did not fit, a header 16 KiB past the bound with an `eyJ`
// every three bytes was read again from each of its five thousand openings
// before the bound.
//
// Timed as linear_test.go's tests are ([withinFactor]): the header of
// openings against one of the same length with none but its first, which
// costs one more read past the bound; read once, the two cost the same.
//
// Mutation: go on looking from the place after the match's start, and this
// fails.
func TestAJSONWebTokensHeaderOfManyOpeningsIsReadOnce(t *testing.T) {
	t.Parallel()
	read := func(text string) { _ = Secrets(text) }
	token := func(header string) string {
		return header + ".eyJ" + strings.Repeat("b", 30) + "." + strings.Repeat("c", 43) + "\n"
	}
	n := MaxReach + 16<<10
	plain := token("eyJ" + strings.Repeat("a", n-3))
	openings := token(strings.Repeat("eyJ", n/3) + "aaaaaaaa")
	if ok, plainCost, openingsCost := withinFactor(read, plain, openings, 3); !ok {
		t.Errorf("%v for a header of openings against %v for one with only its first: the header is "+
			"read again from each opening", openingsCost, plainCost)
	}
}
