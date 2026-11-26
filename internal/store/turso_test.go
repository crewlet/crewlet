package store

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/nativelib"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// What these cases protect.
//
// The Turso driver extracts its ~20 MB native library into a per-user cache,
// writing straight into the final path. A second process looking mid-copy reads
// a partial file, and the driver PANICS on the hash mismatch inside a
// sync.Once, so the process cannot recover and the corrupt file fails every
// later start.
//
// Two properties: concurrent starts must not produce that file, and a cache
// that already holds one must heal rather than wedge the host.

// tursoChildEnv marks a child process, so the helper case below is inert in an
// ordinary run. The cache root itself travels as TURSO_GO_CACHE_DIR, because
// that is the only thing the loader reads.
const tursoChildEnv = "CREWLET_TEST_TURSO_CHILD"

// childRan is printed by every child half and REQUIRED by every parent.
//
// Without it these cases pass vacuously. A parent selects its child with
// `-test.run=^TestSomeChild$` and reads the exit status, and a pattern matching
// NOTHING makes `go test` warn "no tests to run", print PASS and exit 0. So
// renaming a child, or the parent's copy of its name, leaves the parent seeing
// success from a process that asserted nothing.
const childRan = "CREWLET_CHILD_RAN"

// childEnv is the environment a child half runs with: this process's own,
// then the race runtime's exit sleep switched off
// ([procgrouptest.StandInRaceOptions]), then extra.
//
// A child that passes exits 0, and a race-built process that exits 0 sleeps
// atexit_sleep_ms — a second by default — before it ends, with its parent
// waiting on it: a second of nothing on every child, and the broken-cache
// parents are serial cases. Nothing here is about the child's own exit, so
// nothing is given up. An exec keeps the LAST of a repeated key, so this GORACE
// replaces an inherited one — whose options it carries forward — and extra,
// appended after it, wins over both.
func childEnv(extra ...string) []string {
	env := append(os.Environ(), procgrouptest.GORACE+"="+procgrouptest.StandInRaceOptions())
	return append(env, extra...)
}

// requireChildRan fails unless the child both succeeded AND actually ran.
func requireChildRan(t *testing.T, what string, out []byte, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v\n%s", what, err, out)
	}
	if !strings.Contains(string(out), childRan) {
		t.Fatalf("%s: the child exited 0 without running — `go test` reports a "+
			"pass over a -test.run pattern that matches nothing, so this case "+
			"certified nothing. The child test was renamed and its parent's "+
			"copy of the name was not.\n%s", what, out)
	}
}

// TestTursoLibraryPreparedByAChildProcess is the child half of the
// concurrency case: one process preparing one cache root.
func TestTursoLibraryPreparedByAChildProcess(t *testing.T) {
	if os.Getenv(tursoChildEnv) == "" {
		t.Skip("not a child process; see TestConcurrentStartsDoNotCorruptTheLibraryCache")
	}
	// The cache root reaches the child as TURSO_GO_CACHE_DIR, which is the
	// only lever there is: the loader reads it and nothing else.
	if err := prepareTursoLibraryNow(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	fmt.Println(childRan)
}

// CONCURRENT PROCESSES ON ONE COLD CACHE ALL SUCCEED.
//
// Real processes, not goroutines: the state is shared through the filesystem,
// and an in-process mutex would prove nothing about the case that actually
// happens — several engines, or several test binaries, starting at once on one
// host. This is the failure that took the whole suite down in CI, reported as
// a panic out of a package that had merely opened a store.
func TestConcurrentStartsDoNotCorruptTheLibraryCache(t *testing.T) {
	root := t.TempDir()
	const starts = 10

	var wg sync.WaitGroup
	failures := make([]string, starts)
	for i := range starts {
		wg.Go(func() {
			cmd := exec.Command(os.Args[0], //nolint:gosec // os.Args[0] is this test binary
				"-test.run=^TestTursoLibraryPreparedByAChildProcess$", "-test.count=1")
			cmd.Env = childEnv(tursoChildEnv+"=1", nativelib.CacheEnv+"="+root)
			out, err := cmd.CombinedOutput()
			switch {
			case err != nil:
				failures[i] = err.Error() + ": " + string(out)
			case !strings.Contains(string(out), childRan):
				// See [childRan]: a -test.run that matches nothing exits 0.
				failures[i] = "the child exited 0 without running: " + string(out)
			}
		})
	}
	wg.Wait()
	for i, failure := range failures {
		if failure != "" {
			t.Errorf("start %d could not prepare the library: %s", i, failure)
		}
	}
}

// A HALF-WRITTEN CACHE HEALS. Without this the first process killed mid-copy
// — an OOM, a container stop, a Ctrl-C — leaves a file that fails identically
// forever, on every engine on that host, until a human deletes a cache entry
// they have never heard of.
func TestAHalfWrittenLibraryCacheIsHealed(t *testing.T) {
	root := t.TempDir()
	t.Setenv(nativelib.CacheEnv, root)
	if err := prepareTursoLibraryNow(); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	library := theCachedLibrary(t, root)
	whole, err := os.ReadFile(library)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly what an interrupted copy leaves: a prefix of the real file,
	// non-empty, so the size check upstream makes reads it as present.
	//
	// UNLINKED FIRST, NOT TRUNCATED IN PLACE. The preparation above dlopen'd
	// this very file, and shortening a mapping's backing file SIGBUSes the
	// process that holds it — here, this test binary. The replacement is a
	// new inode, which leaves the live mapping alone.
	if err := os.Remove(library); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(library, whole[:len(whole)/3], 0o755); err != nil {
		t.Fatal(err)
	}

	if err := prepareTursoLibraryNow(); err != nil {
		t.Fatalf("a truncated cache was not healed: %v", err)
	}
	healed, err := os.ReadFile(theCachedLibrary(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if len(healed) != len(whole) {
		t.Fatalf("the healed library is %d bytes, want %d", len(healed), len(whole))
	}
}

// theCachedLibrary finds the one extracted library under a cache root.
func theCachedLibrary(t *testing.T, root string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, tursoCacheDirName, "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	var libraries []string
	for _, match := range matches {
		if info, err := os.Stat(match); err == nil && info.Mode().IsRegular() {
			libraries = append(libraries, match)
		}
	}
	if len(libraries) != 1 {
		t.Fatalf("the cache under %s holds %v, want exactly one library", root, libraries)
	}
	return libraries[0]
}

// A CACHE ROOT THAT CANNOT EXIST IS REPORTED AS ITSELF, not as a library that
// would not verify. The second message tells an operator to clear a directory,
// and sending them to clear one that cannot be created is worse than useless.
func TestAnUnusableCacheRootIsReportedAsOne(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(nativelib.CacheEnv, root)
	err := prepareTursoLibraryNow()
	if err == nil {
		t.Fatal("an unusable cache root was accepted")
	}
	if !strings.Contains(err.Error(), "preparing the Turso library cache") {
		t.Fatalf("error = %v; a root that cannot be created is not a library that "+
			"would not verify, and the two need different answers", err)
	}
	if !strings.Contains(err.Error(), root) {
		t.Fatalf("error = %v; it must name the directory an operator has to look at", err)
	}
}

// AND A LIBRARY THAT WILL NOT VERIFY EVEN AFTER A HEAL NAMES THE WAY OUT.
// There used to be two ways out — clear the cache by hand, or move to the
// certified fallback driver — and dropping the second driver
// deleted one of them. That makes the remaining message MORE load-bearing,
// not less: it is now the only thing standing between an operator and a
// binary that will not start, so it must name the cache directory to delete
// and the variable that relocates it, and say plainly that there is nothing
// to fall back to.
func TestALibraryThatWillNotHealSaysWhatToDo(t *testing.T) {
	root := t.TempDir()
	t.Setenv(nativelib.CacheEnv, root)
	if err := prepareTursoLibraryNow(); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	// A cache entry the heal cannot remove: a NON-EMPTY DIRECTORY standing
	// where the library file goes. Upstream stats it, sees a non-zero size,
	// fails to read it as a file — and os.Remove refuses a directory with
	// something in it, so the heal cannot clear the way either.
	library := theCachedLibrary(t, root)
	if err := os.Remove(library); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(library, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := prepareTursoLibraryNow()
	if err == nil {
		t.Fatal("a library that cannot be verified or healed was accepted")
	}
	for _, want := range []string{filepath.Join(root, tursoCacheDirName), nativelib.CacheEnv,
		"no second driver"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v; it must name %q", err, want)
		}
	}
}

// OPEN FAILS RATHER THAN TAKING THE PROCESS DOWN. This is the whole point of
// preparing the library here instead of letting the driver do it on the first
// connection: the driver's answer to a cache it cannot verify is a PANIC, from
// inside a sync.Once that marks itself done even when the function panicked —
// so the process cannot recover, and a caller that would happily have printed
// a remediation never gets the chance.
//
// A child process, because the preparation is memoised per process and this
// has to be the first store this one opens.
func TestOpenReportsABrokenLibraryCacheInsteadOfPanicking(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], //nolint:gosec // os.Args[0] is this test binary
		"-test.run=^TestOpenWithABrokenLibraryCacheInAChildProcess$", "-test.count=1")
	cmd.Env = childEnv(tursoChildEnv+"=1", nativelib.CacheEnv+"="+root)
	out, err := cmd.CombinedOutput()
	requireChildRan(t, "Open did not report the broken cache", out, err)
}

// TestOpenWithABrokenLibraryCacheInAChildProcess is the child half of the case
// above.
func TestOpenWithABrokenLibraryCacheInAChildProcess(t *testing.T) {
	if os.Getenv(tursoChildEnv) == "" {
		t.Skip("not a child process; see TestOpenReportsABrokenLibraryCacheInsteadOfPanicking")
	}
	_, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "state.db"),
		Options{})
	if err == nil {
		t.Fatal("Open succeeded against a library cache that cannot exist")
	}
	if !strings.Contains(err.Error(), os.Getenv(nativelib.CacheEnv)) {
		t.Fatalf("error = %v; it must name the cache an operator has to look at", err)
	}
	fmt.Println(childRan)
}
