package flock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// hangDetector bounds a Try this suite expects to come back at once. Try is
// non-blocking by contract, so one that has not answered in this long has
// blocked — which is the failure, not a slow machine: the honest answer to a
// held lock is "held, by whom", immediately.
const hangDetector = 30 * time.Second

// tryWithin takes path, failing the case if the Try blocks.
func tryWithin(t *testing.T, path string) (*Lock, error) {
	t.Helper()
	type answer struct {
		lock *Lock
		err  error
	}
	answered := make(chan answer, 1)
	go func() {
		lock, err := Try(path)
		answered <- answer{lock, err}
	}()
	select {
	case got := <-answered:
		return got.lock, got.err
	case <-time.After(hangDetector):
		t.Fatalf("a Try of %s has not answered in %v: it blocked on the holder "+
			"instead of saying who holds the lock", path, hangDetector)
		return nil, nil
	}
}

// TWO TAKERS IN ONE PROCESS EXCLUDE EACH OTHER, as two processes do, and the
// second is refused AT ONCE. The broker's stream directory depends on it: two
// embedded brokers in one process on one directory corrupt it exactly as two
// processes would, so a lock that only excluded other processes would let the
// second one start.
//
// Mutation: share the descriptor between takers (or reuse one claim) and the
// second Try succeeds; take the lock blocking (LOCK_EX without LOCK_NB) and
// this case fails at its hang detector.
func TestASecondTakerInThisProcessIsRefused(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x.lock")

	first, err := Try(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer first.Release()

	second, err := tryWithin(t, path)
	if err == nil {
		second.Release()
		t.Fatal("a second taker in this process was handed a lock already held")
	}
	var held *HeldError
	if !errors.As(err, &held) || !errors.Is(err, ErrHeld) {
		t.Fatalf("the refusal is not a HeldError unwrapping to ErrHeld: %v", err)
	}
	if want := fmt.Sprintf("pid %d ", os.Getpid()); !strings.HasPrefix(held.Holder, want) {
		t.Errorf("the refusal names %q, not this process (%q…)", held.Holder, want)
	}
	if held.Path != path {
		t.Errorf("the refusal names %q, want the lock file %q", held.Path, path)
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), held.Holder) {
		t.Errorf("the message %q must name both the lock file and its holder", err)
	}
}

// RELEASING FREES IT for the next taker, and leaves the file where it is.
//
// The file stays because unlinking it races a peer that has opened it and is
// about to lock: that peer would lock an unlinked inode while a third created a
// fresh file and locked that, and both would believe they held it.
//
// And a release is safe twice, from two goroutines at once, and on a nil lock,
// so every caller can defer it without tracking whether it already ran.
//
// Mutation: read and clear the descriptor in two steps rather than one swap,
// and the race detector reports the two releases.
func TestReleasingLetsTheNextTakerInAndKeepsTheFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "x.lock")

	first, err := Try(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	var both sync.WaitGroup
	for range 2 {
		both.Go(first.Release)
	}
	both.Wait()
	first.Release()
	var nothing *Lock
	nothing.Release()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the lock file was removed on release: %v", err)
	}
	second, err := tryWithin(t, path)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	second.Release()
}

// A PATH THAT CANNOT HOLD A LOCK IS NOT A HELD ONE. A lock file whose
// directory cannot be made has no holder to name, and a caller answers the two
// in different words — "stop the process that has it" against "this path is
// wrong" — so the failure is wrapped, names where it failed, and is never
// ErrHeld.
func TestAPathThatCannotHoldALockIsNotAHeldOne(t *testing.T) {
	t.Parallel()
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(notADir, "x.lock")

	lock, err := Try(path)
	if err == nil {
		lock.Release()
		t.Fatal("a lock file under a regular file was locked")
	}
	if errors.Is(err, ErrHeld) {
		t.Fatalf("the failure = %v; a path that cannot hold a lock was reported as held", err)
	}
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("the failure = %v; it must wrap the cause", err)
	}
	if !strings.Contains(err.Error(), notADir) {
		t.Errorf("the failure = %v; it must name the directory it could not prepare", err)
	}
}

// The stamp is the ONE diagnostic a refused taker gets, and a hostname may be
// 253 bytes: a fixed buffer short of that drops the host — the single field
// naming which machine to go and look at.
func TestTheHolderStampIsReadWhole(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lock")
	host := strings.Repeat("h", 253)
	line := "pid 1234567 on " + host + " since 2026-08-31T00:00:00Z\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if got := readHolder(f); got != strings.TrimSpace(line) {
		t.Errorf("readHolder = %q, want the whole stamp", got)
	}
}

// An empty file is the window between a peer's lock and its stamp, or a
// release that left the file behind: "somebody, and we cannot say who".
func TestAnEmptyStampNamesNobody(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := readHolder(f); got != UnknownHolder {
		t.Errorf("readHolder on an empty stamp = %q, want %q", got, UnknownHolder)
	}
}

// And the read is bounded: the path is operator-supplied, so reading an
// arbitrary file whole into an error message is how a mistyped path becomes a
// gigabyte in the heap.
func TestTheStampReadIsBounded(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxHolderStamp*3)), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := readHolder(f); len(got) > maxHolderStamp {
		t.Errorf("readHolder returned %d bytes, past its %d bound", len(got), maxHolderStamp)
	}
}

// A HOLDER REPLACES WHATEVER THE FILE HELD: a stamp left by an earlier holder
// that wrote a longer line must not survive under the new one, or a refusal
// names a process that is long gone.
func TestAStampReplacesTheLastHoldersLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lock")
	stale := "pid 1 on " + strings.Repeat("stale-host-", 30) + " since 2001-01-01T00:00:00Z\n"
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Try(path)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer lock.Release()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "stale-host") {
		t.Errorf("the previous holder's stamp survived: %q", raw)
	}
	if want := fmt.Sprintf("pid %d ", os.Getpid()); !strings.HasPrefix(string(raw), want) {
		t.Errorf("the stamp is %q, want it to name this process (%q…)", raw, want)
	}
}

// THE STAMP SAYS WHICH PROGRAM HOLDS THE LOCK, and never what it was given: a
// refused taker has to tell an engine (`crewlet run`) from a command that lets
// go when it finishes, and a value can follow on a command line.
//
// Mutation: stamp the whole command line and the value case fails; stamp none
// of it and every case does.
func TestTheStampNamesTheProgramAndNotItsArguments(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"/usr/local/bin/crewlet", "run", "-config", "crewlet.yaml"}, "crewlet run"},
		{[]string{"crewlet", "secrets", "set", "GL_TOKEN", "-value", "glpat-x"}, "crewlet secrets set"},
		{[]string{"crewlet", "llm", "login", "default", "-capture-token"}, "crewlet llm login"},
		{[]string{"crewlet"}, "crewlet"},
		{nil, "unknown"},
	} {
		if got := command(c.args); got != c.want {
			t.Errorf("command(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}
