package artifactupload_test

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/static"
)

// A RELEASE SHIPS WHAT ITS DIRECTORY HOLDS, nested files included, and a
// release the tree does not hold ships nothing: the set is read from the tree,
// so a file a later release added is answered for each release as the origin
// would answer it.
func TestAReleaseShipsTheFilesItsDirectoryHolds(t *testing.T) {
	t.Parallel()
	set, err := artifactupload.ReleasesIn(fstest.MapFS{
		"0.32.0/kit.css":                {Data: []byte(":root{}")},
		"0.32.0/fonts/geist-sans.woff2": {Data: []byte("font")},
		"0.33.0/kit.css":                {Data: []byte(":root{}")},
		"0.33.0/content.css":            {Data: []byte("p{}")},
		"notes.txt":                     {Data: []byte("not a release")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		version, file string
		ships         bool
	}{
		{"0.32.0", "kit.css", true},
		{"0.32.0", "fonts/geist-sans.woff2", true},
		{"0.32.0", "content.css", false},
		{"0.32.0", "fonts", false},
		{"0.33.0", "content.css", true},
		{"9.9.9", "kit.css", false},
	} {
		if got := set.Ships(c.version, c.file); got != c.ships {
			t.Errorf("Ships(%q, %q) = %v, want %v", c.version, c.file, got, c.ships)
		}
	}
}

// THE EMBEDDED KIT IS READ AS THE ORIGIN SERVES IT: every release the binary
// holds ships the stylesheet a page links, and the releases from before the
// content stylesheet existed do not ship it, so a link to one is warned.
func TestTheEmbeddedKitReleasesShipWhatTheirTreesHold(t *testing.T) {
	t.Parallel()
	tree, err := fs.Sub(static.FS(), "artifacts/kit")
	if err != nil {
		t.Fatal(err)
	}
	set, err := artifactupload.ReleasesIn(tree)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Versions()) == 0 {
		t.Fatal("the binary embeds no kit release")
	}
	for _, v := range set.Versions() {
		if !set.Ships(v, "kit.css") {
			t.Errorf("kit %s ships no kit.css", v)
		}
	}
	if set.Ships("0.32.0", "content.css") {
		t.Error("kit 0.32.0 ships content.css, which the kit first carried after it")
	}
}
