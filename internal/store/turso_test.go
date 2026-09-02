package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// What these cases protect.
//
// The Turso driver extracts a ~20 MB native library into a per-user cache on
// first use, and writes it straight into the final path instead of renaming a
// temporary file into place. A second process that looks while the first is
// copying reads a partial file, and the driver's response to a hash mismatch
// is to PANIC — inside a sync.Once, which marks itself done even when the
// function panicked, so the process cannot recover either. The corrupt file
// then stays on disk and every later start fails the same way.
//
// Two properties, then: concurrent starts must not be able to produce that
// file, and a cache that already holds one must heal rather than wedge the
// host for good.

// tursoChildEnv marks a child process, so the helper case below is inert in an
// ordinary run. The cache root itself travels as TURSO_GO_CACHE_DIR, because
// that is the only thing the loader reads.
const tursoChildEnv = "CREWLET_TEST_TURSO_CHILD"

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
			cmd.Env = append(os.Environ(),
				tursoChildEnv+"=1", tursoCacheEnv+"="+root)
			if out, err := cmd.CombinedOutput(); err != nil {
				failures[i] = err.Error() + ": " + string(out)
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
	t.Setenv(tursoCacheEnv, root)
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

// A LOCK IS NOT STOLEN WHILE IT IS FRESH — the whole point is that a peer
// mid-extraction is waited for rather than raced.
func TestAFreshLockIsWaitedForRatherThanStolen(t *testing.T) {
	dir := t.TempDir()
	first, held := acquireTursoLock(dir, time.Now().Add(time.Minute))
	if !held {
		t.Fatal("the first caller did not take the lock")
	}
	defer first()

	start := time.Now()
	if _, held := acquireTursoLock(dir, time.Now().Add(150*time.Millisecond)); held {
		t.Fatal("a second caller took a lock the first still holds")
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("the second caller gave up after %v; it must wait for the peer that is "+
			"extracting, or the serialisation buys nothing", waited)
	}
}

// AND A STALE ONE IS RECLAIMED. A lock is held only across an extraction, so
// one older than that belongs to a process that was killed holding it —
// and never reclaiming it would wedge every engine on the host.
func TestAStaleLockIsReclaimed(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, ".crewlet-extract.lock")
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * tursoLockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	release, held := acquireTursoLock(dir, time.Now().Add(time.Second))
	if !held {
		t.Fatal("an abandoned lock was never reclaimed")
	}
	release()
}

// DEBRIS AT THE LOCK PATH IS CLEARED RATHER THAN WAITED FOR. Only this code
// creates that path and it only ever creates a directory, so a file there is
// not a peer — and treating it as one would stall every engine on the host for
// the whole timeout, on this start and on every start after it.
func TestDebrisAtTheLockPathIsCleared(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".crewlet-extract.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	release, held := acquireTursoLock(dir, time.Now().Add(tursoPrepareTimeout))
	if !held {
		t.Fatal("a file at the lock path was treated as a lock somebody holds")
	}
	release()
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("clearing debris took %v; it must not wait out the timeout", waited)
	}
}

// A LOCK THAT CANNOT BE TAKEN AT ALL IS NOT AN ERROR, and is not waited for
// either: there is no peer to wait for when the path cannot hold a lock.
// prepareTursoLibraryNow runs the preparation unguarded in that case, which is
// the behaviour before any of this existed and strictly better than an engine
// that refuses to start.
func TestALockThatCannotBeTakenIsGivenUpAtOnce(t *testing.T) {
	notADirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADirectory, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, held := acquireTursoLock(notADirectory, time.Now().Add(tursoPrepareTimeout)); held {
		t.Fatal("acquireTursoLock claimed a lock it does not hold")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("gave up after %v; a path that cannot hold a lock has no peer to wait for", waited)
	}
}

// A CACHE ROOT THAT CANNOT EXIST IS REPORTED AS ITSELF, not as a library that
// would not verify. The second message tells an operator to clear a directory,
// and sending them to clear one that cannot be created is worse than useless.
func TestAnUnusableCacheRootIsReportedAsOne(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tursoCacheEnv, root)
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
	t.Setenv(tursoCacheEnv, root)
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
	for _, want := range []string{filepath.Join(root, tursoCacheDirName), tursoCacheEnv,
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
	cmd.Env = append(os.Environ(), tursoChildEnv+"=1", tursoCacheEnv+"="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Open did not report the broken cache: %v\n%s", err, out)
	}
}

// TestOpenWithABrokenLibraryCacheInAChildProcess is the child half of the case
// above.
func TestOpenWithABrokenLibraryCacheInAChildProcess(t *testing.T) {
	if os.Getenv(tursoChildEnv) == "" {
		t.Skip("not a child process; see TestOpenReportsABrokenLibraryCacheInsteadOfPanicking")
	}
	_, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.db"),
		Options{})
	if err == nil {
		t.Fatal("Open succeeded against a library cache that cannot exist")
	}
	if !strings.Contains(err.Error(), os.Getenv(tursoCacheEnv)) {
		t.Fatalf("error = %v; it must name the cache an operator has to look at", err)
	}
}
