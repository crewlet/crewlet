// Package sourcetree walks THIS MODULE'S OWN SOURCE, and nothing nested inside
// its directory that is somebody else's.
//
// A dozen source gates walk the repository from its root — the ADR anchors,
// the stored-decode sites, the estate boundary, the stream writers, the env
// file readers — and each of them used [filepath.WalkDir] with its own list of
// directory names to skip. None of those lists could say the one thing that
// matters: a directory below the root that is a CHECKOUT or a MODULE of its
// own is not this module's source, whatever it is called. The repository
// already expects one — `.gitignore` carries `.claude/worktrees/`, the nested
// checkouts an AI coding assistant creates to isolate parallel work — and a
// gate that read one reported the SIBLING copy's files as this tree's: an ADR
// template cited twice, a decode site declared once and found three times,
// every finding a parallel task had not yet fixed failing a branch that had.
//
// THE BOUNDARY IS THE GO TOOL'S OWN, plus git's. `go list ./...` stops at a
// directory holding its own go.mod, because that is another module; git stops
// at a directory holding its own .git, because that is another checkout (a
// worktree carries a .git FILE rather than a directory, which is why the test
// is for the entry rather than for a directory of that name). A gate that
// judged a different set of files from the one the build compiles would be
// certifying something other than what ships.
//
// A LEAF over the standard library, so any package's tests can take it.
package sourcetree

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Walk is [filepath.WalkDir] over root that never descends into a nested
// checkout or module — see the package doc. fn sees every other entry exactly
// as WalkDir would hand it, including root itself, and may return
// [filepath.SkipDir] to prune its own names as before.
func Walk(root string, fn fs.WalkDirFunc) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && path != root {
			nested, statErr := Nested(path)
			if statErr != nil {
				return fn(path, d, statErr)
			}
			if nested {
				return filepath.SkipDir
			}
		}
		return fn(path, d, err)
	})
}

// Nested reports whether dir is the top of another checkout or module: it
// holds a `.git` entry (a directory, or the file a worktree carries) or a
// `go.mod`.
//
// The error is the third value rather than a false, because a directory that
// could not be inspected is not known to be this module's, and a gate that
// walked it anyway would be reading a tree it cannot place.
func Nested(dir string) (bool, error) {
	for _, marker := range []string{".git", "go.mod"} {
		_, err := os.Lstat(filepath.Join(dir, marker))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}
