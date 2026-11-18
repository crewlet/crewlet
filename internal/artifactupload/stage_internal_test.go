package artifactupload

import (
	"net/url"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// A FONT IN A FORMAT NO ARTIFACT SERVES IS REFUSED NAMING THE ONES THAT ARE: a
// stylesheet from the allowlist naming `.eot` is fixed in one more publish only
// when the refusal says which formats to pick.
func TestARefusedFontNamesTheFontsAnArtifactServes(t *testing.T) {
	t.Parallel()
	u, err := url.Parse("https://cdnjs.cloudflare.com/ajax/libs/x/1.0.0/fonts/icons.eot")
	if err != nil {
		t.Fatal(err)
	}
	_, err = vendorPath(lint.Ref{URL: u, Kind: lint.RefFont})
	if err == nil {
		t.Fatal("an .eot font was given a path")
	}
	for _, ext := range mediatype.ExtensionsOf(mediatype.ClassFont) {
		if !strings.Contains(err.Error(), ext) {
			t.Errorf("the refusal %q does not name %s", err, ext)
		}
	}
	if len(mediatype.ExtensionsOf(mediatype.ClassFont)) == 0 {
		t.Fatal("the media table serves no font")
	}
}
