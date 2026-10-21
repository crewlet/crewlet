package cards

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// EVERY CODE GIVEN A SENTENCE IS ONE A REFUSAL CARRIES: the API's, or one the
// assistant raises itself. A row keyed on a code nothing answers is a sentence
// no person reads, which is what a renamed code leaves behind while the code's
// refusals fall back to saying only that they were not accepted. Mutation: key
// a row on a code nothing answers and this fails.
func TestEveryCodeGivenASentenceIsOneARefusalCarries(t *testing.T) {
	t.Parallel()
	known := []string{CodeInvalidCall, CodePreviewUnreadable, CodeDeepUnavailable}
	for _, c := range httpjson.Codes() {
		known = append(known, string(c))
	}
	for code, said := range codeSentences {
		if !slices.Contains(known, code) {
			t.Errorf("%q is given %q, and no refusal carries it", code, said)
		}
		if !ForAPerson(said) {
			t.Errorf("%q reads %q, which is not a sentence a person reads", code, said)
		}
	}
}
