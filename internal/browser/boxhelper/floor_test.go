package boxhelper_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE TREE RECIPE REFUSES THE NPM THE HELPER REFUSES: scripts/playwright-tree.sh
// installs the suite's tree with npm min_npm or newer, and that one line is
// MinNPMMajor, so the tree every case reads was never installed by an npm a box
// may not run, and a raise of the floor moves the two lines together (the
// recipe's own suite reads its npm cases off min_npm). Mutations, each red:
// min_npm=9 or min_npm=11 in the recipe; MinNPMMajor = 11; the line set twice.
func TestTheTreeRecipeRefusesTheNPMTheHelperRefuses(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "scripts", "playwright-tree.sh"))
	if err != nil {
		t.Fatal(err)
	}
	lines := regexp.MustCompile(`(?m)^min_npm=([0-9]+)$`).FindAllSubmatch(body, -1)
	if len(lines) != 1 {
		t.Fatalf("scripts/playwright-tree.sh sets min_npm=<number> %d times; it must set it once", len(lines))
	}
	if got, _ := strconv.Atoi(string(lines[0][1])); got != boxhelper.MinNPMMajor {
		t.Errorf("scripts/playwright-tree.sh installs with npm %d or newer, and boxhelper.MinNPMMajor is %d: move "+
			"min_npm with MinNPMMajor", got, boxhelper.MinNPMMajor)
	}
}
