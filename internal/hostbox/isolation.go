package hostbox

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// Isolation is the environment that keeps a child process's state inside ONE
// home, by name and value: HOME, the XDG base directories and TMPDIR, each
// inside home, and XDG_CACHE_HOME at cache — every place a CLI or a coding
// agent keeps state, so that state is the seat's or the box's rather than the
// engine user's or another seat's. Both consumers set it LAST in a child's
// environment, over every layer an operator or a profile can write.
//
// ONE SPELLING, because there were two and they drifted: the CLI backend
// pointed TMPDIR at `home/tmp` and the local box at `home/.tmp`, the same bug
// (the isolation applied before the layers an operator writes) needed fixing
// once in each, and the CLI's TMPDIR was a directory nothing created — so
// `mktemp`, Node's `mkdtemp` and Go's `os.CreateTemp` failed with ENOENT in
// every text-mode call, probe and login. A variable added to the isolation is
// added here, for both, by one edit, and [Prepare] makes its directory exist.
//
// THE CACHE IS THE CALLER'S, and it is the one deliberate difference: a CLI
// seat's home is pruned between calls, so its cache lives BESIDE the home and
// stays warm, while a box is one run and keeps its cache inside.
func Isolation(home, cache string) map[string]string {
	return map[string]string{
		"HOME":            home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME":  cache,
		"TMPDIR":          TempDir(home),
	}
}

// TempDir is where [Isolation] points TMPDIR for home: a directory of the
// home's own, which a consumer whose home outlives one call empties between
// calls, since temporary files are exactly what nothing else deletes.
func TempDir(home string) string { return filepath.Join(home, ".tmp") }

// Prepare creates every directory an isolation names, at [DirMode]: the
// [Isolation] itself, and whatever a consumer layered on it (a CLI's own
// relocation variables). A child is entitled to find the directory its
// TMPDIR or XDG variable names already there — a program creating a
// temporary file does not create the directory first, and one creating its
// config directory with a single, non-recursive mkdir fails under a parent
// that does not exist. Idempotent, so every call into a shared home may run
// it; in sorted order, so a failure names the same directory every time.
func Prepare(isolation map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(isolation)) {
		dir := isolation[name]
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, DirMode); err != nil {
			return fmt.Errorf("hostbox: preparing %s (%q): %w", name, dir, err)
		}
	}
	return nil
}
