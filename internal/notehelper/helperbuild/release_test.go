package helperbuild_test

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/notehelper/helperbuild"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// EVERY RELEASE ARCHIVE CARRIES BOTH HELPERS BESIDE THE BINARY: an engine from
// an archive looks beside itself, and a box's arch is its own whatever the
// archive's platform, so each archive lists each build by its exact path, at
// its root. goreleaser fails on an exact path that matches nothing, but an
// entry deleted fails nothing, so this holds the list. Mutation, red: drop one
// entry, or give it a dst.
func TestEveryReleaseArchiveCarriesBothHelpersBesideTheBinary(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var release struct {
		Archives []struct {
			ID    string      `yaml:"id"`
			Files []yaml.Node `yaml:"files"`
		} `yaml:"archives"`
	}
	if err = yaml.Unmarshal(raw, &release); err != nil {
		t.Fatal(err)
	}
	if len(release.Archives) == 0 {
		t.Fatal(".goreleaser.yaml declares no archives")
	}
	for _, archive := range release.Archives {
		beside := map[string]bool{}
		for _, node := range archive.Files {
			var file struct {
				Src         string `yaml:"src"`
				Dst         string `yaml:"dst"`
				StripParent bool   `yaml:"strip_parent"`
			}
			if node.Kind == yaml.MappingNode {
				if err = node.Decode(&file); err != nil {
					t.Fatal(err)
				}
			}
			beside[file.Src] = file.Src != "" && file.Dst == "" && file.StripParent
		}
		for _, arch := range helperbuild.Archs {
			if src := "build/notehelper/" + helperbuild.File(arch); !beside[src] {
				t.Errorf("archive %q does not carry %s at its root (src, strip_parent, no dst)", archive.ID, src)
			}
		}
	}
}
