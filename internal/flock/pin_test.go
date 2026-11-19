package flock

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// A pin is the kernel counting the holders of one open file description for
// the engine: what a child's output file needs once no pipe says when its last
// writer let go. These cases hold it to the three things that use relies on.

// A PIN IS HELD BY WHATEVER HOLDS ITS DESCRIPTION, AND BY NOTHING ONCE THEY ARE
// GONE. The description is pinned, the file unlinked, the description handed
// to a child as its stdout and the engine's copy closed. The probe, a second
// description of the same inode, sees the pin while the child lives and none
// once it exits. A real child, because inheriting a description across fork
// and exec is the whole property: a dup here would pass regardless.
//
// Mutation: make Pin take a shared lock and the probe's shared lock is granted
// beside it, so the child's description reads as unpinned; make Pinned read a
// refusal as free and it does too.
func TestAPinIsHeldWhileAChildHoldsItsDescription(t *testing.T) {
	t.Parallel()
	path, w, probe := openTwice(t)
	if err := Pin(w); err != nil {
		t.Fatalf("pin: %v", err)
	}
	// UNLINKED, as a pinned file may be: the lock lives on the inode.
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing %s: %v", path, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), hangDetector)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=NoSuchTest") //nolint:gosec // os.Args[0] is this test binary
	cmd.Env = append(os.Environ(), procgrouptest.GORACE+"="+procgrouptest.StandInRaceOptions(), holdEnv+"=1")
	// AN *os.File, so os/exec hands the child this description itself
	// rather than a pipe it copies from.
	cmd.Stdout = w
	release, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the holder: %v", err)
	}
	// The child has its copy from the fork on; this one goes, so the
	// child's is the only holder left.
	_ = w.Close()

	assertPinned(t, probe, true, "while the child that inherited the description runs")

	_ = release.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the holder: %v", err)
	}
	assertPinned(t, probe, false, "once the only process that held the description has exited")
}

// A PROBE LEAVES NOTHING BEHIND. Asked of a file nothing pins, Pinned answers
// no, and the description it asked through holds no lock afterwards: a pin
// taken next is granted. A probe that kept its shared lock would refuse every
// later pin of the file, and answer every later probe "free" from its own lock.
//
// Mutation: make Pinned return without releasing the shared lock it took, and
// the pin after it is refused.
func TestAProbeLeavesNoLockBehind(t *testing.T) {
	t.Parallel()
	_, w, probe := openTwice(t)

	assertPinned(t, probe, false, "before anything pinned it")
	if err := Pin(w); err != nil {
		t.Fatalf("a pin after a probe that found the file free was refused: %v; "+
			"the probe kept the shared lock it took", err)
	}
	assertPinned(t, probe, true, "once it is pinned")
}

// A FILE ALREADY LOCKED THROUGH ANOTHER DESCRIPTION IS NOT PINNED, AND SAYS SO.
// A pin is taken on a description the caller has just opened; another
// description holding a lock on the file means the file is not the caller's
// alone, and a pin that returned as if it held would count nothing.
//
// Mutation: make Pin answer nil when the lock was refused, and the second pin
// passes.
func TestAPinRefusedByAnotherDescriptionIsAnError(t *testing.T) {
	t.Parallel()
	_, first, second := openTwice(t)
	if err := Pin(first); err != nil {
		t.Fatalf("pin: %v", err)
	}
	err := Pin(second)
	if err == nil {
		t.Fatal("a second description pinned a file another description already pins")
	}
	if !errors.Is(err, ErrHeld) {
		t.Errorf("the refusal %v does not unwrap to ErrHeld", err)
	}
}

// openTwice makes a fresh file and opens it twice: once write-only and
// appending, as a child's output is opened, and once read-write, as the
// engine's own description of it is. Two opens, so two open file
// descriptions of one inode.
func openTwice(t *testing.T) (path string, w, probe *os.File) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "capture")
	w, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, FileMode) //nolint:gosec // the case's own temporary file
	if err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	t.Cleanup(func() { _ = w.Close() })
	probe, err = os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // the case's own temporary file
	if err != nil {
		t.Fatalf("opening %s again: %v", path, err)
	}
	t.Cleanup(func() { _ = probe.Close() })
	return path, w, probe
}

func assertPinned(t *testing.T, probe *os.File, want bool, when string) {
	t.Helper()
	got, err := Pinned(probe)
	if err != nil {
		t.Fatalf("Pinned %s: %v", when, err)
	}
	if got != want {
		t.Fatalf("Pinned %s = %v, want %v", when, got, want)
	}
}

// holdEnv makes this test binary a holder: it keeps the descriptors it was
// started with until its stdin ends, and then exits.
const holdEnv = "CREWLET_TEST_FLOCK_HOLD"

// runHoldHelper holds until stdin ends: the case's word that the holder may go.
func runHoldHelper() int {
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}
