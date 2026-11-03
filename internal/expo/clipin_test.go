package expo_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// easLaunch is an EAS CLI launch through npx, with the release it names, if
// any.
var easLaunch = regexp.MustCompile(`npx\s+(?:--yes\s+|-y\s+)?eas-cli(?:@([^\s` + "`" + `"']+))?`)

// EVERY PAGE AND SKILL THAT LAUNCHES THE EAS CLI NAMES THE ONE PINNED RELEASE,
// so what the docs say an agent runs is what [expo.CLIRelease] records.
func TestEveryEASCLILaunchNamesThePinnedRelease(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	read := 0
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
			read++
			for _, m := range easLaunch.FindAllStringSubmatch(string(body), -1) {
				if m[1] != expo.CLIRelease {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s launches %q; name eas-cli@%s, the release internal/expo pins",
						rel, m[0], expo.CLIRelease)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if read == 0 {
		t.Fatal("read no page, so nothing was checked")
	}
	for _, line := range []string{"npx eas-cli build:list", "npx -y eas-cli@26.0.0 build:list"} {
		if m := easLaunch.FindStringSubmatch(line); m == nil || m[1] == expo.CLIRelease {
			t.Errorf("%q was not read as a launch of another release", line)
		}
	}
}
