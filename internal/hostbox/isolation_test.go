package hostbox

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// THE ISOLATION IS ONE SPELLING, pinned here so a name added to it is a
// reviewed change to both consumers at once: HOME, the XDG base directories
// and TMPDIR inside the home, and the cache wherever the caller keeps it.
// The CLI backend's seats and a direct box both set exactly this
// (cliprofile.TestTheSeatBaseIsTheHostIsolation,
// sandbox.TestADirectBoxsIsolationIsTheHostIsolation).
func TestTheIsolationIsOneSpelling(t *testing.T) {
	t.Parallel()
	got := Isolation("/h", "/c")
	want := []string{"HOME", "TMPDIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"}
	if names := slices.Sorted(maps.Keys(got)); !slices.Equal(names, want) {
		t.Fatalf("the isolation names %v, want %v", names, want)
	}
	if got["XDG_CACHE_HOME"] != "/c" {
		t.Errorf("XDG_CACHE_HOME = %q, want the caller's cache", got["XDG_CACHE_HOME"])
	}
	if got["TMPDIR"] != TempDir("/h") {
		t.Errorf("TMPDIR = %q, want TempDir's %q", got["TMPDIR"], TempDir("/h"))
	}
	for name, dir := range got {
		if name != "XDG_CACHE_HOME" && dir != "/h" && !strings.HasPrefix(dir, "/h/") {
			t.Errorf("%s = %q, outside the home", name, dir)
		}
	}
}

// EVERY DIRECTORY THE ISOLATION NAMES EXISTS once it is prepared, private to
// the engine user — the CLI backend's TMPDIR was a directory nothing created,
// so every temporary file a CLI made failed with ENOENT — and preparing it
// again is harmless, since every call into a shared home does.
//
// Mutation: skip a name in Prepare, and its directory is missing.
func TestPrepareCreatesEveryDirectoryTheIsolationNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	isolation := Isolation(filepath.Join(root, "home"), filepath.Join(root, "cache"))
	isolation["VENDOR_DIR"] = filepath.Join(root, "home", ".vendor")
	for range 2 {
		if err := Prepare(isolation); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
	}
	for name, dir := range isolation {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("%s (%q) was not created: %v", name, dir, err)
			continue
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s (%q) is %o, readable beyond the engine user", name, dir, perm)
		}
	}
	file, err := os.CreateTemp(isolation["TMPDIR"], "probe-*")
	if err != nil {
		t.Fatalf("a temporary file in the prepared TMPDIR: %v", err)
	}
	_ = file.Close()
}
