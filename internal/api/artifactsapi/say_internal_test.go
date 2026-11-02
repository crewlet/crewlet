package artifactsapi

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REFUSAL IS TOLD IN ITS SENTENCE, wherever its package names and its class
// words sit.
//
// This surface trimmed package names from the FRONT of a refusal and nothing
// else, so a field refused by the domain read "invalid: kind …", a manifest's
// path "invalid: manifest: invalid: tracker: the path …", and a lost race
// carried the framework's error and the broker subject after the domain's
// sentence. A lost race is told in the state log's words for a person, as on
// every surface; the rest in the publish pipeline's sentence, and this
// surface's own refusals without its name.
//
// Mutation: trim from the front only again, or drop the statelog arm, and a row
// goes red.
func TestARefusalIsToldInItsSentence(t *testing.T) {
	t.Parallel()
	_, badPath := manifest.CleanPath("pages//index.html")
	if badPath == nil {
		t.Fatal("the path this case wraps was not refused")
	}
	for name, c := range map[string]struct {
		err  error
		says string
	}{
		"a field": {fmt.Errorf("%w: kind %q is not one of %v", artifactsdomain.ErrInvalid,
			"zine", artifacts.Kinds), `kind "zine" is not one of`},
		"a manifest's path": {fmt.Errorf("%w: %w", artifactsdomain.ErrInvalid, badPath),
			`the path "pages//index.html" has an empty folder in it`},
		"the pipeline's": {fmt.Errorf("%w: a publish names its artifact",
			artifactupload.ErrInvalid), "a publish names its artifact"},
		"a lost race": {fmt.Errorf("%w: a-1: %w", artifactsdomain.ErrConflict,
			fmt.Errorf("%w: crewlet.artifacts.artifact.a-1 kept changing under this write",
				statelog.ErrConflict)), statelog.LostRace},
		"the origin's": {artifactserve.ErrNoDocument, "the frame names no page of the version"},
	} {
		said := say(c.err)
		if said != c.says && !strings.HasPrefix(said, c.says) {
			t.Errorf("%s is told %q, want it to open %q", name, said, c.says)
		}
		for _, leak := range []string{"artifactsdomain", "artifactupload", "artifactserve",
			"manifest:", "worklet:", "statelog", "invalid:", "crewlet."} {
			if strings.Contains(said, leak) {
				t.Errorf("%s is told with %q in it: %s", name, leak, said)
			}
		}
	}
}
