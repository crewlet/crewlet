package static_test

import (
	"io/fs"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactupload"
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
	"0.26.0",
	"0.27.0",
	"0.27.1",
	"0.28.0",
	"0.29.0",
	"0.31.1",
	"0.32.0",
	"0.33.0",
	"0.34.1",
	"0.35.1",
	"0.36.0",
	"0.37.0",
	"0.37.1",
	"0.38.0",
	"0.39.0",
	"0.42.0",
	"0.43.0",
	"0.47.0",
	"0.48.3",
	"0.49.1",
	"0.50.0",
	"0.51.0",
	"0.54.0",
	"0.55.0",
	"0.56.0",
	"0.57.1",
	"0.57.2",
	"0.58.0",
	"0.60.0",
	"0.61.0",
	"0.63.0",
	"0.64.0",
	"0.66.0",
	"0.67.0",
	"0.68.0",
	"0.69.0",
	"0.70.0",
	"0.71.0",
	"0.72.0",
	"0.73.0",
	"0.74.0",
	"0.75.0",
	"0.75.1",
	"0.75.2",
	"0.75.3",
	"0.75.4",
	"0.76.0",
	"0.76.1",
	"0.77.0",
	"0.78.0",
	"0.79.0",
	"0.80.0",
	"0.81.0",
	"0.81.1",
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

// TestTheNewestKitIsTheLedgersLast: the ledger ends on the newest kit this
// build embeds, ordered as the publish pipeline orders releases. That release
// is what the engine tells every seat to link (`${crewlet_kit_version}`, the
// embedded release set's newest) and what get_company_brand answers, so a kit
// appended anywhere but the end, or embedded without its ledger line, would
// leave the skill naming a release the record says is not the newest.
func TestTheNewestKitIsTheLedgersLast(t *testing.T) {
	t.Parallel()
	tree, err := fs.Sub(static.FS(), "artifacts/kit")
	if err != nil {
		t.Fatalf("the embedded kit tree: %v", err)
	}
	kits, err := artifactupload.ReleasesIn(tree)
	if err != nil {
		t.Fatalf("reading the embedded kit tree: %v", err)
	}
	if got, want := kits.Latest(), shippedKits[len(shippedKits)-1]; got != want {
		t.Fatalf("the newest embedded kit is %q and the ledger ends on %q: append each "+
			"new kit at the end of shippedKits, oldest first", got, want)
	}
	if got := kits.Versions(); !slices.Equal(got, shippedKits) {
		t.Fatalf("the embedded kits in release order are %v and the ledger reads %v: "+
			"keep the ledger in release order", got, shippedKits)
	}
}
