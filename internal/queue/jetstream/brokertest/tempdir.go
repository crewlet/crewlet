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
)

// TempDir is t.TempDir for a directory a test's broker keeps its store in,
// at the directory itself or anywhere under it: a fresh, empty directory,
// which t removes, with everything under it, once it ends. A fixture keeps
// the rest of a node beside the store there too (its database, its Tier A),
// as it would in t.TempDir.
//
// ONCE IT ENDS, AFTER EVERY BROKER STARTED IN IT HAS STOPPED: the removal is
// registered when the directory is made, before a broker can start in it, and
// a test's cleanups run last-registered first, so the stop a fixture registers
// once its broker has started runs before the removal.
//
// NOT t.TempDir ITSELF, whose removal tries once and fails the test on any
// error, while a stopped nats-server can still write its store. Every
// file-backed consumer flushes its state from a goroutine nats-server starts
// beside the consumer's store (consumerFileStore.flushLoop), which neither
// Shutdown nor WaitForShutdown joins: a consumer's stop waits for a flush in
// flight only by polling for 100 ms (waitOnFlusher), and on a loaded runner
// the write — the state file's temporary, renamed over the state file under
// obs/<consumer> — lands after both have returned. A removal that read that
// directory before the write landed finds it not empty at the end, and a case
// failed on a file it never wrote (crewlet/crewlet#1079; internal/queue/
// jetstream's shutdownAndClean measures the race).
//
// So the removal is TRIED AGAIN WHEN, AND ONLY WHILE, IT FAILS WITH ENOTEMPTY.
// Each such failure means a file landed after the attempt read its directory,
// and the next attempt removes it; and once a consumer's directory is gone, a
// write that comes later lands nowhere, since nats-server writes the state
// file into that directory and never makes it. Nothing is waited for between
// attempts: this is a retry on the event, not a pause. The attempts are
// bounded ([storeRetries]); a directory still not empty after them is being
// written by something that has not stopped, and the test fails naming the
// directory and what remained in it. Any other failure fails it at once, as
// t.TempDir's would.
//
// The retry, and this note, go once the pinned nats-server joins a consumer's
// flush loop in its stop rather than polling for it, which takes the late
// writes to none (shutdownAndClean has the measurement): a test's store can
// then live in t.TempDir like any other file.
func TempDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "crewlet-test-store-") //nolint:usetesting // t.TempDir's own removal is the one this replaces: a late flush fails it
	if err != nil {
		t.Fatalf("make a directory for a test broker's store: %v", err)
	}
	t.Cleanup(func() {
		if err := removeStore(dir, os.RemoveAll); err != nil {
			t.Error(err)
		}
	})
	return dir
}

// storeRetries is how many times [TempDir]'s removal is tried again after a
// late write interrupted it, before the test fails.
//
// AN ATTEMPT FAILS ONLY WHEN A WRITE LANDS INSIDE IT, between its reading a
// directory and its removing that directory, and one late write can fail two
// attempts at most: one that meets its temporary file, and one that meets the
// rename of that file over the state file. The attempt after them removes the
// directory, and the write has nowhere left to land. A consumer makes one such
// write at most, since a stopped consumer refuses every write but the one it
// found in flight (nats-server v2.15.1 on). So sixteen retries outlast eight
// consumers caught mid-write by one stop, where the worst measured was a write
// still in flight at 3 stops of 1,528 (shutdownAndClean). A directory still not
// empty after them is being written by something that never stopped (a broker
// the case did not shut down), which no number of attempts outlasts. Nothing
// is waited for between them, so the bound costs nothing until it is reached.
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
