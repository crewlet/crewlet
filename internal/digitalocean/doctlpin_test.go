package digitalocean_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/digitalocean"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// doctlRelease is a doctl release a page names: the version a recipe sets, or
// one in a release asset's name.
var doctlRelease = regexp.MustCompile(`doctl_version=([0-9][0-9.]*)|doctl-([0-9]+\.[0-9]+\.[0-9]+)-`)

// EVERY PAGE AND SKILL THAT INSTALLS DOCTL NAMES THE ONE PINNED RELEASE, so
// what the docs say an agent runs is what [digitalocean.DoctlRelease] records.
func TestEveryDoctlInstallNamesThePinnedRelease(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	found := 0
	for _, dir := range []string{"docs", "skills", "examples"} {
		err := sourcetree.Walk(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".yaml") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range doctlRelease.FindAllStringSubmatch(string(body), -1) {
				found++
				if got := m[1] + m[2]; got != digitalocean.DoctlRelease {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s installs doctl %s; name %s, the release internal/digitalocean pins",
						rel, got, digitalocean.DoctlRelease)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if found == 0 {
		t.Fatal("no page installs doctl, so the pin guards nothing: docs/integrations/digitalocean.md should")
	}
}
