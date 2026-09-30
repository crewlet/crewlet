// Command gofiles prints every Go source file in this repository's tree, one
// path per line relative to the module root, by [sourcetree.Walk]'s rule — so
// that gofmt reads exactly what the guards read, and nothing a nested
// checkout holds.
//
//	files="$(go run ./internal/sourcetree/gofiles)" && gofmt -l $files
//
// # Why gofmt needs it
//
// `gofmt -l .` enters EVERY directory under the one it is given — not `.git`,
// not a dot-directory, not a nested checkout is skipped. With an agent's
// worktree under .claude/worktrees/, `make fmt-check` reported that
// worktree's half-edited files as this tree's, and `make fmt`, which is
// `gofmt -w .`, REWROTE them: one checkout's formatter reaching into another
// that a different process was in the middle of editing.
//
// # Why a program and not a shell filter
//
// The rule is [internal/sourcetree]'s, and a `find` expression beside it
// would be a second copy of "which directories are ours" — the thing that
// package exists to have one of. And the program FAILS rather than printing
// nothing, which a filter cannot promise: `gofmt -l` handed no paths at all
// formats its standard input, finds nothing wrong with it, and reports a clean
// tree. A failure stops gofmt only when the list is captured first, as above
// and as the Makefile and ci.yml do: written inline as `gofmt -l $(…)`, the
// shell discards the substitution's exit status and gofmt reads stdin anyway.
//
// It prints the files gofmt's own walk would have picked, which excludes a
// name starting with a dot: gofmt skips those, and a formatter that started
// reading them would be a change nobody asked for.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

func main() {
	files, err := list(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "gofiles:", err)
		os.Exit(1)
	}
	for _, file := range files {
		fmt.Println(file)
	}
}

// list is every Go file under root, which must be the module root.
func list(root string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		// The paths it prints are relative to where it ran, and gofmt
		// resolves them against the same directory — so anywhere but the
		// root prints paths that are right only by accident.
		return nil, fmt.Errorf("run this from the module root, where the "+
			"Makefile and ci.yml run it: %w", err)
	}
	var files []string
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir(), !strings.HasSuffix(d.Name(), ".go"), strings.HasPrefix(d.Name(), "."):
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", root, err)
	}
	if len(files) == 0 {
		return nil, errors.New("no Go files in this tree, so a gofmt check " +
			"handed this list would read its standard input and pass")
	}
	return files, nil
}
