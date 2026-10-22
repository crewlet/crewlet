package iamdomain

import (
	"errors"
	"strings"

	"github.com/crewlet/crewlet/internal/iam"
)

// Sentence is an error this package wrote, as a person reads it: a
// refusal's sentence without its class word or what it was about, and a
// value's refusal without the sentinel it opens with.
//
// A REFUSAL READS "not allowed: <sentence> (<about>)" in a log, and the two
// halves around the sentence are for the log alone: the class word is what
// errors.Is finds and the status already says, and [Refusal.About] names the
// ids an operator needs and the person never chose. A value's refusal opens
// with its sentinel ("invalid login: \"Jane\" isn't a valid person login"),
// which says what [errors.Is] finds and nothing a reader acts on. Both go,
// wherever they sit, and every word after them stays.
//
// THE ONE PLACE THESE WORDS ARE CUT, beside the sentinels, as
// internal/artifactsdomain cuts its own: the identity directory's surface and
// the sign-in surface each answered err.Error(), which put the class word in
// front of every refusal and, before [Refusal.About], an invitation's id, a
// session's lineage or a person's id in the middle of it.
func Sentence(err error) string {
	text := err.Error()
	var refused *Refusal
	if errors.As(err, &refused) && refused.Err != nil {
		text = strings.Replace(text, refused.Error(), refused.Err.Error(), 1)
	}
	for _, cut := range sentenceCuts {
		text = strings.ReplaceAll(text, cut, "")
	}
	return text
}

// sentenceCuts are the sentinels [Sentence] removes from the front of the
// sentence that follows them: the value refusals a surface answers 400.
var sentenceCuts = []string{
	ErrInvalid.Error() + ": ", ErrInvalidLogin.Error() + ": ",
	ErrInvalidToken.Error() + ": ", ErrNotFindable.Error() + ": ",
}

// grantWords is a set of permissions as a sentence lists them — "tracker,
// knowledge" — and never Go's "[tracker knowledge]", which is how a slice
// printed with %v reads.
func grantWords(grants []iam.Grant) string {
	if len(grants) == 0 {
		return "no permission"
	}
	words := make([]string, len(grants))
	for i, g := range grants {
		words[i] = string(g)
	}
	return strings.Join(words, ", ")
}
