package iamdomain

import (
	"errors"
	"strings"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
)

// Sentence is an error this package wrote, as a person reads it: a
// refusal's sentence without its class word or what it was about, and a
// value's refusal without the sentinel it opens with.
//
// In "not allowed: <sentence> (<about>)" the class word is what errors.Is
// finds and the status already says, and [Refusal.About] names ids for an
// operator that the person never chose; a value refusal's sentinel ("invalid
// login: ...") likewise says nothing a reader acts on. Both go, wherever they
// sit, and every word after them stays. This is the one place the iam
// surfaces cut them, beside the sentinels, as internal/artifactsdomain does.
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

// grantWords lists a party's permissions for a sentence, and "no
// permission" for none: [words.And]'s "none" would read as holding nothing.
func grantWords(grants []iam.Grant) string {
	if len(grants) == 0 {
		return "no permission"
	}
	return words.And(grants)
}
