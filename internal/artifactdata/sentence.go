package artifactdata

import (
	"strings"

	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
)

// Sentence is a datastore's refusal as a person and a model read it: the
// sentence, without the package names and class words a Go error carries in
// front of it (this package's, a query's, a value's and the artifact's
// [artifactsdomain.Sentence]).
//
// ONE FUNCTION FOR EVERY SURFACE: one stripping only package names leaves the
// class word ("refused that: invalid: …"), one cutting a single prefix leaves
// the rest. Not in the applier's files, which read no other domain.
//
// A PURGED STORE IS SAID WITHOUT ITS ARTIFACT'S ID, which [ErrGone]'s error
// names for the log ([artifactsdomain.CutArtifactID]); so is an undo on an
// artifact whose datastore was never written ([ErrNotFound]).
func Sentence(err error) string {
	text := artifactsdomain.CutArtifactID(artifactsdomain.CutArtifactID(
		artifactsdomain.Sentence(err), ErrGone), ErrNotFound)
	for _, cut := range sentenceCuts {
		text = strings.ReplaceAll(text, cut, "")
	}
	return text
}

// sentenceCuts are what [Sentence] removes beyond the artifact's own, the class
// words before the package names they open with.
var sentenceCuts = []string{
	ErrInvalid.Error() + ": ", ErrForbidden.Error() + ": ", query.ErrInvalid.Error() + ": ",
	"artifactdata: ", "query: ", "canon: ",
}
