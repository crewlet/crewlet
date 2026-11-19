package brokertest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/crewlet/crewlet/internal/testbinary"
)

// TempDir is t.TempDir for a directory a test's broker keeps its store in:
// fresh, removed by t AFTER EVERY BROKER STARTED IN IT HAS STOPPED. The removal
// is registered before any broker can start, and cleanups run last-first.
//
// NOT t.TempDir ITSELF, whose single removal fails while a stopped nats-server
// can still write: a consumer's flush goroutine (consumerFileStore.flushLoop) is
// joined by neither Shutdown nor WaitForShutdown, only polled 100 ms
// (waitOnFlusher), so its write can land after the removal read the directory
// (crewlet/crewlet#1079; jetstream's shutdownAndClean). So the removal is TRIED
// AGAIN WHEN, AND ONLY WHILE, IT FAILS WITH ENOTEMPTY, up to [storeRetries]; any
// other failure fails at once. Once nats-server joins the flush loop, t.TempDir
// serves, with jetstream.ErrStoreOutsideTempDir and its gate test.
func TempDir(t testing.TB) string {
	t.Helper()
	return tempDir(t, os.RemoveAll)
}

// tempDir is [TempDir] with the removal its cleanup tries given as removeAll,
// which is os.RemoveAll for every test broker. A test of the cleanup itself
// hands it a removal that lands a late write inside an attempt, and a t that
// records what the cleanup reports, so that it can hold the one cleanup every
// broker test gets — the retry and the report both — rather than a copy of it.
func tempDir(t testing.TB, removeAll func(string) error) string {
	t.Helper()
	// NAMED WITH testbinary.StoreDirPrefix, which is how a test binary tells
	// a store directory TempDir made from any other: an embedded broker
	// started in no such directory is refused (jetstream.ErrStoreOutsideTempDir).
	//
	// MADE WHERE t.TempDir MAKES ITS DIRECTORIES: under GOTMPDIR where that is
	// set, as the testing package reads it, and the system's temporary
	// directory otherwise. A store under the system's while a case's other
	// files sat under GOTMPDIR put the two on two filesystems wherever GOTMPDIR
	// names another, and a rename from one into the other — a git pack written
	// from a case's work tree into its node's repository (gittest.Pack) —
	// failed with EXDEV.
	dir, err := os.MkdirTemp(os.Getenv("GOTMPDIR"), testbinary.StoreDirPrefix) //nolint:usetesting // t.TempDir's own removal is the one this replaces: a late flush fails it
	if err != nil {
		t.Fatalf("make a directory for a test broker's store: %v", err)
	}
	t.Cleanup(func() {
		if err := removeStore(dir, removeAll); err != nil {
			t.Error(err)
		}
	})
	return dir
}

// storeRetries is how many times [TempDir]'s removal is tried again after a
// late write interrupted it, before the test fails.
//
// AN ATTEMPT FAILS ONLY WHEN A WRITE LANDS INSIDE IT, between its reading a
// directory and removing it, and one late write fails two attempts at most:
// one meeting its temporary file, one meeting its rename over the state file.
// The next removes the directory. A consumer makes one such write at most (a
// stopped consumer refuses all but the one in flight, nats-server v2.15.1 on),
// so sixteen retries outlast eight consumers caught mid-write by one stop; the
// worst measured was a write in flight at 3 stops of 1,528 (shutdownAndClean).
// A directory still not empty after them is written by something never stopped,
// which no number of attempts outlasts. Nothing is waited for between them.
const storeRetries = 16

// removeStore removes dir with removeAll, trying again when, and only while,
// an attempt fails with ENOTEMPTY, at most [storeRetries] times ([TempDir]
// says why). It reports a directory it could not remove, naming it and, when
// files kept landing, what remained in it.
func removeStore(dir string, removeAll func(string) error) error {
	err := removeAll(dir)
	for range storeRetries {
		if !errors.Is(err, syscall.ENOTEMPTY) {
			break
		}
		err = removeAll(dir)
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.ENOTEMPTY):
		return fmt.Errorf("the test broker's directory %s was still being written after %d "+
			"attempts to remove it, each meeting a file that landed after the attempt read its "+
			"directory, so something still writes there (a broker the case never stopped?): %w. "+
			"What remained: %s", dir, storeRetries+1, err, residue(dir))
	default:
		return fmt.Errorf("remove the test broker's directory %s: %w", dir, err)
	}
}

// residue is what remained in dir after the last attempt to remove it: every
// file under it, relative to it, which is what landed while that attempt ran.
func residue(dir string) string {
	var left []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Gone since the directory above it was read.
			return nil
		case err != nil:
			return err
		case d.IsDir():
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		left = append(left, filepath.ToSlash(rel))
		return nil
	})
	switch {
	case err != nil:
		return fmt.Sprintf("unknown, since the directory could not be read (%v)", err)
	case len(left) == 0:
		return "no file, since the last attempt"
	}
	return strings.Join(left, ", ")
}
