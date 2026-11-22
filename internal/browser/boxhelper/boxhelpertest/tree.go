package boxhelpertest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// treeEnv is the variable the make targets that run the whole suite name the
// installed Playwright tree in (the Makefile's PLAYWRIGHT_TREE_ENV, from
// scripts/playwright-tree.sh); nothing the engine runs reads it.
const treeEnv = "CREWLET_TEST_PLAYWRIGHT_TREE"

// treeRemedy is the words of a case that lacks the tree: what installs it, and
// a one-package loop's two statements, as floorRemedy's.
const treeRemedy = "`make playwright-tree` installs it and `scripts/playwright-tree.sh` prints its path, which the " +
	"make targets that run the whole suite name here; for the box helper's packages, as two statements: " +
	"`tree=$(scripts/playwright-tree.sh) && " + treeEnv + "=\"$tree\" go test ./internal/browser/boxhelper/...`"

// Tree is the Playwright tree the suite installed from the committed lock
// (internal/browser/boxhelper/tree): a directory holding npm's install of it in
// node_modules and the npm cache that install filled in npm-cache (npm's
// _cacache), its .complete written last, and its package-lock.json the
// committed one byte for byte, so a tree of another lock is never read as this
// one's.
func Tree(t testing.TB) string {
	t.Helper()
	tree := prereqtest.NamedDir(t, treeEnv, "the Playwright tree the suite installs from the committed lock, which a "+
		"case of the box helper reads and installs again", treeRemedy)
	if _, err := os.Stat(filepath.Join(tree, ".complete")); err != nil {
		t.Fatalf("%s=%s is no whole tree, holding no .complete (%v): %s", treeEnv, tree, err, treeRemedy)
	}
	if info, err := os.Stat(filepath.Join(tree, "npm-cache", "_cacache")); err != nil || !info.IsDir() {
		t.Fatalf("%s=%s holds no npm cache (npm-cache/_cacache), the cache its install filled: %s", treeEnv, tree,
			treeRemedy)
	}
	committed, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "internal", "browser", "boxhelper", "tree",
		"package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if installed, err := os.ReadFile(filepath.Join(tree, "package-lock.json")); err != nil ||
		!bytes.Equal(installed, committed) {
		t.Fatalf("%s=%s was installed from another lock than internal/browser/boxhelper/tree's: %s", treeEnv,
			tree, treeRemedy)
	}
	return tree
}
