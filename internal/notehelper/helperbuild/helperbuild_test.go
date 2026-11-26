package helperbuild_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/notehelper/helperbuild"
	"github.com/crewlet/crewlet/internal/notehelper/helpersource"
)

// THE COMMITTED DIGEST IS THE HELPER'S SOURCE AS IT STANDS: a change to any
// package of this module the helper links, a new import included, fails here
// until the digest is regenerated, so an engine never accepts a helper built
// from other source under the same digest. Mutation, red: change a file the
// helper builds from.
func TestTheCommittedDigestIsTheHelpersSourceAsItStands(t *testing.T) {
	t.Parallel()
	got, err := helperbuild.SourceDigest(t.Context(), helperbuild.GoCommand())
	if err != nil {
		t.Fatal(err)
	}
	if got != helpersource.Digest {
		t.Errorf("the note helper's source is %s, but %s holds %s: run `go run ./scripts/notehelper -digest` "+
			"from the module's root and commit it", got, helperbuild.DigestFile, helpersource.Digest)
	}
}
