package brokertest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A STORE A LATE FLUSH LANDED IN IS REMOVED BY THE NEXT ATTEMPT. The first
// attempt is the race crewlet/crewlet#1079 met: it removes what it read of a
// consumer's directory, the consumer's flush lands its state file there, and
// the directory's own removal fails with the operating system's ENOTEMPTY.
// The removal is not reported, and the store is gone.
//
// Mutation: try the removal once, and the first attempt's ENOTEMPTY is
// reported here.
func TestAStoreALateFlushLandedInIsRemovedByTheNextAttempt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	consumer := storeWithAConsumer(t, dir)
	attempts := 0
	err := removeStore(dir, func(path string) error {
		attempts++
		if attempts == 1 {
			return flushLandsDuringRemoval(consumer)
		}
		return os.RemoveAll(path)
	})
	if err != nil {
		t.Fatalf("a store a late flush landed in was reported rather than removed: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the store is still there after its removal (stat: %v)", err)
	}
	if attempts != 2 {
		t.Errorf("the removal was attempted %d times, want 2: the interrupted one and the one "+
			"that removed the late file", attempts)
	}
}

// A STORE THAT KEEPS BEING WRITTEN FAILS THE CASE, NAMING IT AND WHAT REMAINED,
// after the bounded attempts and not one more. Every attempt here meets a file
// that landed after it read the directory, as against a broker the case never
// stopped; past the bound the writer stops, so a removal that went on trying
// ends rather than spinning, and is told by its count.
//
// Mutation: drop the bound (try while the store is not empty), and the removal
// goes past it, finds the store gone and reports nothing.
func TestAStoreThatKeepsBeingWrittenFailsTheCaseNamingIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	consumer := storeWithAConsumer(t, dir)
	attempts := 0
	err := removeStore(dir, func(path string) error {
		attempts++
		if attempts > storeRetries+1 {
			return os.RemoveAll(path)
		}
		return flushLandsDuringRemoval(consumer)
	})
	if attempts != storeRetries+1 {
		t.Errorf("the removal was attempted %d times, want the first and its %d retries",
			attempts, storeRetries)
	}
	if err == nil {
		t.Fatal("a store something kept writing was reported removed")
	}
	if !errors.Is(err, syscall.ENOTEMPTY) {
		t.Errorf("the report does not carry the last attempt's ENOTEMPTY: %v", err)
	}
	late := "stream/jetstream/$G/streams/S/obs/C/o.dat"
	for _, want := range []string{dir, late} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the report does not name %s: %v", want, err)
		}
	}
}

// ONLY A STORE THAT WAS NOT EMPTY IS TRIED AGAIN. Any other failure (a
// permission, a path that is not a directory) is no late write, and another
// attempt would meet it the same way; it is reported at once, as t.TempDir
// would report it.
//
// Mutation: try again on any error, and the refusal is attempted past the
// first time; or report nothing but ENOTEMPTY, and the refusal goes unsaid.
func TestOnlyAStoreThatWasNotEmptyIsTriedAgain(t *testing.T) {
	t.Parallel()
	refused := &fs.PathError{Op: "unlinkat", Path: "stream", Err: syscall.EACCES}
	attempts := 0
	err := removeStore(t.TempDir(), func(string) error {
		attempts++
		return refused
	})
	if attempts != 1 {
		t.Errorf("a refused removal was attempted %d times, want once", attempts)
	}
	if !errors.Is(err, syscall.EACCES) {
		t.Errorf("the refusal was reported as %v, want the removal's EACCES", err)
	}
}

// A STORE GOES WITH ITS TEST, AFTER THE BROKER STARTED IN IT HAS STOPPED. The
// directory is fresh and empty when the case is handed it, and the stop a
// fixture registers once its broker has started runs before the removal, so
// what the stop writes goes with the directory rather than outliving it.
//
// Mutation: register no removal, or remove the directory when TempDir is
// called, and the directory outlives its test.
func TestAStoreGoesWithItsTestAfterItsBrokerStops(t *testing.T) {
	t.Parallel()
	var dir string
	t.Run("case", func(t *testing.T) {
		dir = TempDir(t)
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("the case was handed a directory holding %d entries (read: %v), want a "+
				"fresh, empty one", len(entries), err)
		}
		// THE BROKER'S STOP, registered after the directory as a fixture's
		// is, writing the store's last file as it runs.
		t.Cleanup(func() {
			consumer := filepath.Join(dir, "stream", "jetstream", "$G", "streams", "S", "obs", "C")
			if err := os.MkdirAll(consumer, 0o750); err != nil {
				t.Error(err)
				return
			}
			if err := os.WriteFile(filepath.Join(consumer, "o.dat"), []byte("last"), 0o600); err != nil {
				t.Error(err)
			}
		})
	})
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the store's directory outlived its test (stat: %v)", err)
	}
}

// storeWithAConsumer makes dir hold what a stopped broker storing at
// dir/stream leaves, cut to one consumer's state file, and is that
// consumer's directory.
func storeWithAConsumer(t *testing.T, dir string) string {
	t.Helper()
	consumer := filepath.Join(dir, "stream", "jetstream", "$G", "streams", "S", "obs", "C")
	if err := os.MkdirAll(consumer, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consumer, "o.dat"), []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	return consumer
}

// flushLandsDuringRemoval is one attempt to remove a store that a consumer's
// late flush interrupts: the attempt removes the state file it read in the
// consumer's directory, the flush lands a new one there, and the directory's
// own removal meets it, failing with the operating system's own ENOTEMPTY.
func flushLandsDuringRemoval(consumer string) error {
	state := filepath.Join(consumer, "o.dat")
	if err := os.Remove(state); err != nil {
		return err
	}
	if err := os.WriteFile(state, []byte("late"), 0o600); err != nil {
		return err
	}
	return os.Remove(consumer)
}
