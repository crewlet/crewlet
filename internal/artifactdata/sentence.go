package artifactdata

import (
	"strings"

	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
)

// Sentence is a datastore's refusal as a person and a model read it: the
// sentence, without the package names and the class words a Go error carries
// in front of it — this package's ("artifactdata: invalid: ", "artifactdata:
// forbidden: "), a query's, a value's and the artifact's
// ([artifactsdomain.Sentence]).
//
// ONE FUNCTION FOR EVERY SURFACE, as [artifactsdomain.Sentence] is for the
// artifacts': the tools stripped the package names and left the class word, so
// a refusal read "write_artifact_data refused that: invalid: …", and the
// datastore's API cut one prefix from the front and left the rest.
//
// NOT IN THE APPLIER'S FILES, which read no other domain: it is a surface's
// wording of what a writer or a reader refused, and the artifact's words are
// another domain's.
//
// A PURGED STORE IS SAID WITHOUT ITS ARTIFACT'S ID, which [ErrGone]'s error
// names for the log, as the artifact's own purge is
// ([artifactsdomain.CutArtifactID]); and so is an undo on an artifact whose
// datastore was never written, which [ErrNotFound]'s names the same way.
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
