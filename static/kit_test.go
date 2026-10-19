package static_test

import (
	"io/fs"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/static"
)

// shippedKits is every artifact kit version this engine has embedded, oldest
// first. APPEND ONLY.
//
// A page links the kit by an exact version (`/_crewlet/kit/0.18.0/kit.css`),
// and a published version's files are immutable, so a page written against a
// kit keeps asking for that kit for as long as the version exists. Dropping a
// directory here would leave every such page unstyled, with nothing failing
// until a reader opened one. The runtime build never removes a kit directory
// it no longer writes (`dashboard/vite.runtime.config.ts`), and this ledger is
// what holds that in review: removing a version means removing it here too.
//
// Moving the dashboard to a new @crewlethq/artifact-kit release adds its
// version at the end, after `make dashboard` has written the directory.
var shippedKits = []string{
	"0.18.0",
	"0.20.0",
	"0.23.0",
	"0.25.0",
}

// TestEveryShippedKitVersionStaysEmbedded: each version in the ledger is still
// in the binary with the three files a page links, and the tree holds no kit
// directory the ledger does not name, so a new version cannot arrive without
// joining the record of what pages may pin.
func TestEveryShippedKitVersionStaysEmbedded(t *testing.T) {
	t.Parallel()

	for _, version := range shippedKits {
		for _, file := range []string{"kit.css", "kit.js", "theme.js"} {
			if _, err := fs.Stat(static.FS(), "artifacts/kit/"+version+"/"+file); err != nil {
				t.Errorf("kit %s is in the ledger and the embed holds no %s for it: %v\n"+
					"a published page that links /_crewlet/kit/%s/ would lose its styles; "+
					"restore static/artifacts/kit/%s from history", version, file, err, version, version)
			}
		}
	}

	entries, err := fs.ReadDir(static.FS(), "artifacts/kit")
	if err != nil {
		t.Fatalf("reading the embedded kit tree: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Errorf("artifacts/kit/%s is a file; the tree holds one directory per kit version", entry.Name())
			continue
		}
		if !slices.Contains(shippedKits, entry.Name()) {
			t.Errorf("artifacts/kit/%s is embedded and not in shippedKits; "+
				"append it to the ledger in static/kit_test.go", entry.Name())
		}
	}
}
