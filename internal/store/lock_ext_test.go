package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// The lock's real property, exercised across a real process boundary.
//
// lock_test.go covers the pieces from inside the package. This file covers
// the only thing that matters in production: a SECOND OS PROCESS opening the
// database an engine is running on — which is what `crewlet secrets`,
// `crewlet llm export -secret-store` and every provisioner's secret-store
// sink do as their documented gesture. Two handles inside one process are
// safe and deliberately allowed, so an in-process case would test the wrong
// thing.

// A SECOND PROCESS IS REFUSED, and told which file and by whom.
//
// A SUBPROCESS, because that is the property: two handles inside one process
// are safe and deliberately allowed (see lock.go), so an in-process case
// would either test the wrong thing or force the lock to refuse the
// legitimate one. The test binary re-execs itself with a marker, which is the
// only way to get a genuinely separate process here.
func TestASecondProcessIsRefused(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")

	first, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer func() { _ = first.Close() }()

	out, err := openInSubprocess(t, path)
	if err == nil {
		t.Fatalf("a second process opened the database: %s", out)
	}
	if !strings.Contains(out, lockedMarker) {
		t.Fatalf("the second process failed for some other reason: %s", out)
	}
	if !strings.Contains(out, path) {
		t.Errorf("the refusal does not name the file: %s", out)
	}
	if !strings.Contains(out, "pid") {
		t.Errorf("the refusal does not say who holds it: %s", out)
	}
}

// AND IT IS FREED WHEN THE HOLDER EXITS, however it exits. That is the whole
// reason this is an OS lock rather than a pid file: nothing has to reap it.
func TestTheLockIsFreedWhenTheHolderIsGone(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")

	// A subprocess takes it and exits without closing cleanly.
	if out, err := runHelper(t, path, "abandon"); err != nil {
		t.Fatalf("helper: %v: %s", err, out)
	}
	db, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("open after the holder exited: %v — the lock outlived its "+
			"process and nothing can clear it", err)
	}
	_ = db.Close()
}

// TWO HANDLES IN ONE PROCESS SHARE THE CLAIM. The hazard is a second binary;
// two pools here are the same as one pool with more connections, and refusing
// them would break the legitimate case — a lease whose mutual exclusion has
// to live in the SQL statement rather than in a pool.
func TestTwoHandlesInOneProcessShareTheLock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")

	a, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	b, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("second handle in the same process was refused: %v", err)
	}

	// Closing one must NOT free the claim: the other is still serving.
	if err := a.Close(); err != nil {
		t.Fatalf("close a: %v", err)
	}
	out, err := openInSubprocess(t, path)
	if err == nil {
		t.Fatalf("closing one of two handles freed the lock: %s", out)
	}
	// AND REFUSED BY THE LOCK, not by the driver tripping over the
	// connections the surviving handle still holds. Without this the case
	// passes against a released lock for the wrong reason.
	if !strings.Contains(out, lockedMarker) {
		t.Fatalf("the second process was refused by something other than the "+
			"lock, so the refcount is untested: %s", out)
	}

	// The last close does free it.
	if err := b.Close(); err != nil {
		t.Fatalf("close b: %v", err)
	}
	if out, err := openInSubprocess(t, path); err != nil {
		t.Fatalf("the last close did not free the lock: %v: %s", err, out)
	}
}

// --- the subprocess harness ------------------------------------------- //
//
// Re-execing the test binary is the only way to get a second OS process, and
// the second process is the only thing the lock is about.

// lockedMarker is what the helper prints on a lock refusal. Its own string
// rather than matching ErrLocked's text, so the case fails when the two stop
// agreeing instead of quietly passing on a substring.
const lockedMarker = "REFUSED-BY-LOCK"

// helperEnv names the database the helper should open, and helperModeEnv what
// it should do with it.
const (
	helperEnv     = "CREWLET_TEST_LOCK_DB"
	helperModeEnv = "CREWLET_TEST_LOCK_MODE"
)

// openInSubprocess runs the helper against path and returns its output.
func openInSubprocess(t *testing.T, path string) (string, error) {
	t.Helper()
	return runHelper(t, path, "")
}

func runHelper(t *testing.T, path, mode string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=NoSuchTest")
	cmd.Env = append(os.Environ(),
		helperEnv+"="+path, helperModeEnv+"="+mode,
		// The driver choice has to match, or the helper opens the file
		// with a different engine than the case that locked it.
		store.DriverEnv+"="+os.Getenv(store.DriverEnv))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runLockHelper opens the database and reports what happened. Called from
// TestMain in store_test.go, which is this binary's single entry point.
func runLockHelper(path, mode string) int {
	db, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		if errors.Is(err, store.ErrLocked) {
			fmt.Printf("%s %v\n", lockedMarker, err)
			return 3
		}
		fmt.Printf("open failed: %v\n", err)
		return 4
	}
	if mode == "abandon" {
		// EXIT WITHOUT CLOSING, which is the crash this lock has to
		// survive. The kernel is what frees it.
		return 0
	}
	_ = db.Close()
	return 0
}
