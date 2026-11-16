package redact

import (
	"slices"
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

// A JSON WEB TOKEN'S HEADER OF MANY OPENINGS IS READ ONCE ([jwtWithin]),
// counted by the table's JWT rule asking its bound: twice, not once an opening.
// Mutation: go on looking past the match's start, and it is asked thousands of times.
func TestAJSONWebTokensHeaderOfManyOpeningsIsReadOnce(t *testing.T) {
	t.Parallel()
	i := slices.IndexFunc(rules(), func(r rule) bool { return r.pattern.String() == jwtPattern })
	if i < 0 || rules()[i].bound == nil {
		t.Fatal("the rules hold no JSON Web Token's rule held to its bound")
	}
	matches := func(text string) (int, string) {
		jwt := rules()[i]
		bound, asked := jwt.bound, 0
		jwt.bound = func(text string, start, end int) (bool, int) {
			asked++
			return bound(text, start, end)
		}
		out := jwt.apply(text)
		return asked, out
	}
	token := func(header string) string {
		return header + ".eyJ" + strings.Repeat("b", 30) + "." + strings.Repeat("c", 43) + "\n"
	}
	n := MaxReach + 16<<10
	plain := token("eyJ" + strings.Repeat("a", n-3))
	openings := token(strings.Repeat("eyJ", n/3) + "aaaaaaaa")
	if asked, out := matches(plain); asked != 1 || out != plain {
		t.Errorf("a header with only its first opening was matched %d times and redacted to %d bytes of "+
			"its %d; want once, and nothing taken, its claims too far from the opening", asked, len(out), len(plain))
	}
	asked, out := matches(openings)
	if asked != 2 {
		t.Errorf("a header of %d openings was matched %d times; want twice, from its first opening and "+
			"from the one its claims fit from: the header is read again from each opening", n/3, asked)
	}
	if !strings.Contains(out, rules()[i].with) || len(out) >= len(openings) {
		t.Errorf("the header of openings was redacted to %d bytes of its %d, want the token its claims fit "+
			"taken", len(out), len(openings))
	}
}
