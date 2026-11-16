package coord_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/coord"
)

// A BUILD NAME IS CUT TO ITS BOUND, AT A WHOLE CHARACTER. It is the one field
// of a node's status no table bounds, and the presence's bound is a sum over
// every field, so the cut is what makes that bound total; cut in the middle
// of a character it would carry an invalid byte the encoder spells as six.
//
// Mutation: drop the cut from NodeStatus.Meta and the long build rides whole;
// drop the walk back to a character's start and the cut ends mid-character.
func TestABuildNameIsCutToItsBoundAtAWholeCharacter(t *testing.T) {
	t.Parallel()
	for name, build := range map[string]string{
		"ASCII past the bound":          strings.Repeat("b", 3*coord.MaxBuildBytes),
		"a character across the bound":  strings.Repeat("b", coord.MaxBuildBytes-1) + "é" + "tail",
		"three-byte characters past it": strings.Repeat("€", coord.MaxBuildBytes),
		"exactly the bound":             strings.Repeat("b", coord.MaxBuildBytes),
	} {
		got, _ := coord.NodeStatus{Build: build}.Meta()["build"].(string)
		switch {
		case len(got) > coord.MaxBuildBytes:
			t.Errorf("%s: carried %d bytes of the build, past %d", name, len(got), coord.MaxBuildBytes)
		case !utf8.ValidString(got):
			t.Errorf("%s: the cut left half a character: %q", name, got)
		case !strings.HasPrefix(build, got):
			t.Errorf("%s: carried %q, which is not the start of %q", name, got, build)
		case len(got) < coord.MaxBuildBytes-utf8.UTFMax+1:
			t.Errorf("%s: cut to %d bytes, more than one character short of %d", name, len(got),
				coord.MaxBuildBytes)
		}
	}
	if got := (coord.NodeStatus{Build: "v1.2.3"}).Meta()["build"]; got != "v1.2.3" {
		t.Errorf("a build under the bound is carried as %v, want it whole", got)
	}
}
