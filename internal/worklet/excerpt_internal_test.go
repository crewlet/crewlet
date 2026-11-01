package worklet

import "testing"

// A CHOOSING ANSWER'S EXCERPT IS RECOGNISED BY WHAT WROTE IT: the reader that
// labels an excerpt as quoting an asker's option ([ExcerptQuotesAChoice]) asks
// of exactly the line [choiceExcerpt] composes, with a body and without one,
// and of nothing an answerer writes in their own words.
//
// Mutation: change [choicePrefix] in one of the two and the other stops
// matching it — which is why there is one constant.
func TestAChoosingAnswersExcerptIsRecognisedByWhatWroteIt(t *testing.T) {
	t.Parallel()
	for _, line := range []string{choiceExcerpt("Ship it", ""), choiceExcerpt("Ship it", "agreed")} {
		if !ExcerptQuotesAChoice(line) {
			t.Errorf("%q, a choosing answer's line, is not read as quoting the asker", line)
		}
	}
	for _, own := range []string{"", "agreed, ship it", "I chose to ship it"} {
		if ExcerptQuotesAChoice(own) {
			t.Errorf("%q, an answerer's own words, is read as quoting the asker", own)
		}
	}
}
