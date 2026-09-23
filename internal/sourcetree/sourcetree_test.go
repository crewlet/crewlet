package sourcetree_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// TestAWalkStopsAtAnotherCheckoutOrModule is the whole package: a nested
// worktree (a .git FILE), a nested clone (a .git directory) and a nested
// module (a go.mod) are each somebody else's source, while the root — which
// holds both markers itself — and an ordinary directory are this module's.
func TestAWalkStopsAtAnotherCheckoutOrModule(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/root\n")
	write(".git/HEAD", "ref: refs/heads/main\n")
	write("internal/own/own.go", "package own\n")
	write(".claude/worktrees/wf-1/.git", "gitdir: /elsewhere\n")
	write(".claude/worktrees/wf-1/internal/own/own.go", "package own\n")
	write("vendor-clone/.git/HEAD", "ref: refs/heads/main\n")
	write("vendor-clone/x.go", "package x\n")
	write("tools/go.mod", "module example.com/tools\n")
	write("tools/tool.go", "package tools\n")

	var seen []string
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			seen = append(seen, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"go.mod", "internal/own/own.go"}
	slices.Sort(seen)
	if !slices.Equal(seen, want) {
		t.Fatalf("walked %v; want exactly this module's own files %v", seen, want)
	}
}
