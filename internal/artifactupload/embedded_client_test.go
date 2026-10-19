package artifactupload_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/static"
)

// THE EMBEDDED ARTIFACT TREE SERVES WHAT THE DASHBOARD BUILDS.
//
// The artifact origin serves the page runtime from `artifacts/runtime/<contract>/`
// and the design system's kit from `artifacts/kit/<version>/`, both out of this
// package's embed, and the upload pipeline pins every new version to the newest
// contract embedded (artifactupload.ReleasesIn). The dashboard's third build
// writes both trees, so a source that moved the contract or the kit's version
// without a rebuild would leave the bridge telling a page one contract while
// the origin serves another, and every page that pinned the new one a 404. So
// this reads the contract the source declares, and the kit version the
// dashboard pins, and holds the embed to both.
func TestTheEmbeddedArtifactTreeServesWhatTheDashboardBuilds(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	contract, err := clientsource.Scalar(tree, "RUNTIME_CONTRACT")
	if err != nil {
		t.Fatal(err)
	}
	runtimes, err := fs.Sub(static.FS(), "artifacts/runtime")
	if err != nil {
		t.Fatal(err)
	}
	set, err := artifactupload.ReleasesIn(runtimes)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Serves(contract) {
		t.Errorf("the dashboard builds runtime contract %s and the embed serves %v: "+
			"run `make dashboard` and commit static/artifacts", contract, set.Versions())
	}
	if latest := set.Latest(); latest != contract {
		t.Errorf("a new version would pin contract %s, the newest embedded, and the "+
			"dashboard's bridge speaks %s", latest, contract)
	}
	if _, err := fs.Stat(static.FS(), "artifacts/runtime/"+contract+"/runtime.js"); err != nil {
		t.Errorf("contract %s embeds no runtime.js: %v", contract, err)
	}

	raw, err := os.ReadFile(filepath.Join(filepath.Dir(tree), "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("dashboard/package.json: %v", err)
	}
	kit := manifest.Dependencies["@crewlethq/artifact-kit"]
	if kit == "" {
		t.Fatal("dashboard/package.json pins no @crewlethq/artifact-kit, so the kit the " +
			"origin serves is pinned by nothing")
	}
	for _, file := range []string{"kit.js", "kit.css", "theme.js"} {
		if _, err := fs.Stat(static.FS(), "artifacts/kit/"+kit+"/"+file); err != nil {
			t.Errorf("the dashboard pins kit %s and the embed holds no %s for it: %v "+
				"(run `make dashboard` and commit static/artifacts)", kit, file, err)
		}
	}
}
