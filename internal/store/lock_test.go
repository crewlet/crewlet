package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WHAT THE LOCK IS FOR.
//
// "One file, one process" was a rule the package doc stated and nothing
// enforced. The driver does not support two processes on a database file and
// does not refuse the second opener either, so the failure was silent — and
// `crewlet secrets`, `crewlet llm export -secret-store` and every
// provisioner's secret-store sink opened the engine's live file from a second
// OS process as their documented gesture.

// RELEASING FREES IT for the next opener. A lock that outlived its holder
// would make a clean restart impossible.
func TestReleasingLetsTheNextProcessIn(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")

	first, err := lockStore(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	first.release()

	second, err := lockStore(path)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	second.release()
}

// THE SIDECAR SURVIVES A RELEASE. Unlinking it races a peer that has already
// opened it and is about to lock: that peer would lock an unlinked inode
// while a third created a new file and locked that, and both would believe
// they held it.
func TestReleasingLeavesTheSidecarInPlace(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")

	lock, err := lockStore(path)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	lock.release()

	if _, err := os.Stat(path + lockSuffix); err != nil {
		t.Fatalf("the sidecar was removed on release: %v — a peer mid-open "+
			"would lock an inode nobody else can see", err)
	}
}

// THE STAMP NAMES THIS PROCESS, which is the whole of what a refused opener
// has to go on.
func TestTheStampIdentifiesTheHolder(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")

	lock, err := lockStore(path)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer lock.release()

	raw, err := os.ReadFile(path + lockSuffix)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	pid, ok := pidOf(strings.TrimSpace(string(raw)))
	if !ok {
		t.Fatalf("the stamp carries no pid: %q", raw)
	}
	if pid != os.Getpid() {
		t.Fatalf("stamp pid = %d, want this process (%d)", pid, os.Getpid())
	}
}

// AN IN-MEMORY DATABASE TAKES NO LOCK and writes no sidecar. It is
// per-connection by construction, so there is nothing to exclude — and a
// lock file beside a path that is not a file would land in the working
// directory of whatever ran the test.
func TestAnInMemoryDatabaseNeedsNoLock(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"", ":memory:", ":memory:?cache=shared"} {
		lock, err := lockStore(path)
		if err != nil {
			t.Fatalf("%q: %v", path, err)
		}
		if lock != nil {
			t.Fatalf("%q: took a file lock for an in-memory database", path)
		}
		// And releasing the nil lock is safe, so every caller can defer it
		// unconditionally rather than branching on the path.
		lock.release()
	}
}

// CLOSING FREES IT, so a restart works and a test can reopen its own store.
func TestClosingReleasesTheDatabaseForTheNextOpen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")

	first, err := Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	second, err := Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("reopen after close: %v — a clean restart is impossible", err)
	}
	_ = second.Close()
}

// A FAILED OPEN RELEASES THE LOCK IT TOOK. A handle that never reached a
// caller has no Close coming, so the lock would outlive the attempt for the
// life of the process — and the next honest open would be refused by a
// database nobody is using.
//
// The failure has to land AFTER the lock to test anything: an unknown driver
// is refused before it, so a case built on one would pass against a stranded
// lock. A DIRECTORY is the shape that locks cleanly and then cannot be
// opened as a database.
func TestAFailedOpenDoesNotStrandTheLock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "not-a-database")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := Open(t.Context(), path, Options{})
	if err == nil {
		t.Fatal("a directory was opened as a database")
	}
	if errors.Is(err, ErrLocked) {
		t.Fatal("the open was refused by the lock rather than by the driver, " +
			"so this case proves nothing about the release")
	}
	// The lock must be free: nothing holds this path any more.
	lock, lockErr := lockStore(path)
	if lockErr != nil {
		t.Fatalf("the failed open stranded its lock: %v", lockErr)
	}
	lock.release()
}
