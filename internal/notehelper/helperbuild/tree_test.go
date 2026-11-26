package helperbuild_test

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/notehelper/helperbuild"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// NO HELPER BUILD SITS IN THE SOURCE TREE: a build is made from the source
// beside the engine, never committed, and one committed stays in every
// clone's history after a later commit deletes it. Only the build's own
// outputs hold one, each ignored by .gitignore: `make notehelper`'s beside
// ./crewlet at the module root, and goreleaser's build/ and dist/. Mutation,
// red: put a notehelper-linux-* file anywhere else.
func TestNoHelperBuildSitsInTheSourceTree(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	made := map[string]bool{}
	for _, arch := range helperbuild.Archs {
		made[helperbuild.File(arch)] = true
	}
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		switch {
		case d.IsDir() && (rel == "build" || rel == "dist"):
			return fs.SkipDir
		case d.IsDir(), !strings.HasPrefix(d.Name(), "notehelper-linux-"), made[rel]:
			return nil
		}
		t.Errorf("%s is a note helper build in the source tree: delete it, and never commit one "+
			"(`make notehelper` builds beside ./crewlet, goreleaser into build/)", rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
