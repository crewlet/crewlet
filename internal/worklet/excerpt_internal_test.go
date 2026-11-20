package worklet

import "testing"

// A CHOOSING ANSWER'S WORDS ARE RECOGNISED BY WHAT WROTE THEM: the reader that
// labels a comment or an excerpt as quoting an asker's option ([QuotesAChoice])
// and the check that an answer states its choice ([StatesChoice]) ask of
// exactly what [ChoiceWords] composes, with or without a note, and of nothing
// an answerer writes. Mutation: change [choicePrefix] in one of them and the
// others stop matching it, hence one constant.
func TestAChoosingAnswersWordsAreRecognisedByWhatWroteThem(t *testing.T) {
	t.Parallel()
	for _, line := range []string{ChoiceWords("Ship it", ""), ChoiceWords("Ship it", " agreed ")} {
		if !QuotesAChoice(line) {
			t.Errorf("%q, a choosing answer's words, is not read as quoting the asker", line)
		}
		if !StatesChoice(line, "Ship it") {
			t.Errorf("%q, a choosing answer's words, is not read as stating its choice", line)
		}
		if StatesChoice(line, "Hold it") {
			t.Errorf("%q is read as stating a choice it did not make", line)
		}
	}
	if got, want := ChoiceWords("Ship it", " agreed "), "Chose “Ship it”: agreed"; got != want {
		t.Errorf("ChoiceWords with a note = %q, want %q", got, want)
	}
	for _, own := range []string{"", "agreed, ship it", "I chose to ship it"} {
		if QuotesAChoice(own) {
			t.Errorf("%q, an answerer's own words, is read as quoting the asker", own)
		}
	}
}
