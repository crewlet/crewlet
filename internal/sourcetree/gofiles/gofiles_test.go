package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, body := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// GOFMT IS HANDED THIS TREE'S GO FILES AND NOTHING ELSE.
//
// A nested checkout is the reason this exists: `gofmt -w .` rewrote files in
// another agent's worktree. So the case that matters is a worktree carrying
// a Go file, beside the files gofmt's own walk would already have skipped —
// a dot-named file — and the ones it reads everywhere, testdata included.
func TestOnlyThisTreesGoFilesAreListed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, map[string]string{
		"go.mod":                                  "module example.com/m\n",
		"cmd/tool/main.go":                        "package main\n",
		"internal/pkg/pkg.go":                     "package pkg\n",
		"internal/pkg/pkg_test.go":                "package pkg\n",
		"internal/pkg/testdata/fixture.go":        "package fixture\n",
		"internal/pkg/.scratch.go":                "package pkg\n",
		"internal/pkg/notes.md":                   "# not Go\n",
		".claude/worktrees/wf_1/.git":             "gitdir: /elsewhere\n",
		".claude/worktrees/wf_1/internal/half.go": "package  half\n",
		"dashboard/node_modules/pkg/gen.go":       "package gen\n",
	})
	// Changing directory is what `go run` from the Makefile does; the
	// function takes the root instead so the case can run in parallel.
	got, err := list(root)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for i, path := range got {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		got[i] = filepath.ToSlash(rel)
	}
	slices.Sort(got)
	want := []string{
		"cmd/tool/main.go",
		"internal/pkg/pkg.go",
		"internal/pkg/pkg_test.go",
		"internal/pkg/testdata/fixture.go",
	}
	if !slices.Equal(got, want) {
		t.Errorf("listed %q, want %q", got, want)
	}
}

// NOTHING TO LIST IS A FAILURE, never an empty line: gofmt handed no paths
// formats its standard input, finds nothing wrong, and the check passes.
// Run from anywhere but the root is the same failure one step earlier.
func TestAListThatWouldCertifyNothingIsRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		files map[string]string
		says  string
	}{
		"no Go files":         {map[string]string{"go.mod": "module example.com/m\n"}, "no Go files"},
		"not the module root": {map[string]string{"pkg/pkg.go": "package pkg\n"}, "module root"},
	} {
		root := t.TempDir()
		write(t, root, tc.files)
		got, err := list(root)
		if err == nil {
			t.Errorf("%s: listed %q, want a refusal", name, got)
			continue
		}
		if !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: refusal %q does not say %q", name, err, tc.says)
		}
	}
}
