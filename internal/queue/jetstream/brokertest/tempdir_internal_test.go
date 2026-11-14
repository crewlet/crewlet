package brokertest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// A TEST BROKER'S STORE IS MADE WHERE THE CASE'S TEMPORARY DIRECTORIES ARE:
// under GOTMPDIR where that is set, as t.TempDir makes them. Made under the
// system's temporary directory while a case's other files sat under GOTMPDIR,
// a store was on another filesystem wherever GOTMPDIR names one, and a git
// pack written from a case's work tree into its node's repository renamed
// across the two and failed (internal/gitlet/host's landing cases).
//
// NOT PARALLEL: it sets GOTMPDIR.
//
// Mutation: make the store under os.TempDir again, and it is not under the
// GOTMPDIR the case set.
func TestATestBrokersStoreIsMadeWhereTheCasesTempDirsAre(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GOTMPDIR", root)
	if dir := TempDir(t); filepath.Dir(dir) != root {
		t.Errorf("TempDir made %s, want a directory directly under GOTMPDIR %s", dir, root)
	}
}

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

// TEMPDIR'S CLEANUP IS THE REMOVAL THAT OUTLASTS A LATE FLUSH, and every
// broker test gets it through TempDir: the removal the cleanup registered
// meets a consumer's flush landing inside its first attempt, as a stopped
// broker's store did in crewlet/crewlet#1079, tries again, and leaves no
// directory and nothing reported.
//
// Mutation: have the cleanup remove the store once — removeAll(dir), or
// os.RemoveAll(dir), the single removal t.TempDir makes — and the late flush
// is reported, or never met.
func TestTempDirsCleanupRemovesAStorePastALateFlush(t *testing.T) {
	t.Parallel()
	rec := &recorder{TB: t}
	var consumer string
	attempts := 0
	dir := tempDir(rec, func(path string) error {
		attempts++
		if attempts == 1 {
			return flushLandsDuringRemoval(consumer)
		}
		return os.RemoveAll(path)
	})
	sweep(t, dir)
	consumer = storeWithAConsumer(t, dir)
	rec.end()
	if len(rec.errs) != 0 {
		t.Errorf("the cleanup reported a store a late flush landed in rather than removing it: %q",
			rec.errs)
	}
	if attempts != 2 {
		t.Errorf("the cleanup attempted its removal %d times, want 2: the one the late flush "+
			"interrupted and the one that removed the late file", attempts)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the store outlived its cleanup (stat: %v)", err)
	}
}

// TEMPDIR'S CLEANUP FAILS THE CASE ON A STORE IT COULD NOT REMOVE, once and
// naming the directory: one still being written once the bounded attempts
// are spent, which it reports with the file that landed last, and one whose
// removal was refused for any other reason, which it reports at once with
// the refusal. A cleanup that said nothing would pass a case whose broker
// never stopped.
//
// Mutation: have the cleanup drop the removal's error, and neither row is
// reported; drop the bound, and the first row's removal goes on until the
// writer stops and reports nothing.
func TestTempDirsCleanupFailsTheCaseOnAStoreItCannotRemove(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// removal is the cleanup's removal for a store whose one consumer
		// keeps its state in consumer.
		removal func(consumer string) func(string) error
		// names is what the report says beside the directory.
		names []string
	}{
		{
			name: "a store still being written past the bound",
			removal: func(consumer string) func(string) error {
				attempts := 0
				return func(path string) error {
					attempts++
					if attempts > storeRetries+1 {
						// The writer stops, so a removal that went
						// past the bound ends rather than spinning.
						return os.RemoveAll(path)
					}
					return flushLandsDuringRemoval(consumer)
				}
			},
			names: []string{"stream/jetstream/$G/streams/S/obs/C/o.dat"},
		},
		{
			name: "a store whose removal is refused",
			removal: func(string) func(string) error {
				return func(string) error {
					return &fs.PathError{Op: "unlinkat", Path: "stream", Err: syscall.EACCES}
				}
			},
			names: []string{syscall.EACCES.Error()},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{TB: t}
			// The removal is made once the store holds its consumer, which
			// is after tempDir registered it: a broker writes its store
			// only once it has the directory.
			var removal func(string) error
			dir := tempDir(rec, func(path string) error { return removal(path) })
			sweep(t, dir)
			removal = tc.removal(storeWithAConsumer(t, dir))
			rec.end()
			if len(rec.errs) != 1 {
				t.Fatalf("the cleanup reported %d errors, want the one naming the store: %q",
					len(rec.errs), rec.errs)
			}
			for _, want := range append([]string{dir}, tc.names...) {
				if !strings.Contains(rec.errs[0], want) {
					t.Errorf("the report does not name %s: %s", want, rec.errs[0])
				}
			}
		})
	}
}

// recorder is the testing.TB a case hands [tempDir], so that the case runs
// the cleanup tempDir registers and reads back what it reported, where the
// case's own T would fail the case itself: it keeps every cleanup registered
// on it and every error reported to it, and is the case's T for anything
// else.
type recorder struct {
	testing.TB
	cleanups []func()
	errs     []string
}

func (r *recorder) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }

func (r *recorder) Error(args ...any) { r.errs = append(r.errs, fmt.Sprint(args...)) }

func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// end runs the cleanups registered on r as a test's end runs them, the last
// registered first.
func (r *recorder) end() {
	for _, f := range slices.Backward(r.cleanups) {
		f()
	}
}

// sweep removes dir once t has ended, whatever the cleanup a case drives
// through a [recorder] did with it, so a case whose removal stopped short
// leaves nothing in the machine's temporary directory. Nothing writes there
// by then, so one removal is the whole of it.
func sweep(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
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
