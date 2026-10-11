package filelock_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/filelock"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// The claim's real property, across a real process boundary.
//
// filelock_test.go holds the pieces inside one process, which flock allows
// because a claim belongs to an open file description. What production is
// guarded against is a SECOND OS PROCESS — a CLI pointed at the store a
// running engine holds, a second engine pointed at the same directory — and
// what makes this an OS lock rather than a pid file is that the KERNEL frees
// it when the holder dies. Neither can be shown without a second process, so
// the test binary re-execs itself with a marker.

// TestMain dispatches the helper: the one entry point this binary has, so a
// re-exec carrying the marker runs the helper and never the suite.
func TestMain(m *testing.M) {
	if path := os.Getenv(helperEnv); path != "" {
		os.Exit(runClaimHelper(path, os.Getenv(helperModeEnv)))
	}
	os.Exit(m.Run())
}

// A SECOND PROCESS IS REFUSED, and told which path and by whom.
func TestASecondProcessIsRefused(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "claim.lock")

	claim, err := filelock.Take(path)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer claim.Release()

	out, err := runHelper(t, path, "")
	if err == nil {
		t.Fatalf("a second process took a held claim: %s", out)
	}
	if !strings.Contains(out, heldMarker) {
		t.Fatalf("the second process failed for some other reason: %s", out)
	}
	if !strings.Contains(out, path) {
		t.Errorf("the refusal does not name the path: %s", out)
	}
	if want := fmt.Sprintf("is held by pid %d", os.Getpid()); !strings.Contains(out, want) {
		t.Errorf("the refusal does not name the holder (%q): %s", want, out)
	}
}

// AND IT IS FREED WHEN THE HOLDER IS GONE, however it went. A subprocess takes
// the claim and exits WITHOUT releasing it — the crash this lock has to
// survive — and the next claimant gets it: nothing has to reap it, which is
// the whole reason this is an OS lock rather than a pid file.
func TestTheClaimIsFreedWhenTheHolderIsGone(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "claim.lock")

	if out, err := runHelper(t, path, "abandon"); err != nil {
		t.Fatalf("helper: %v: %s", err, out)
	} else if !strings.Contains(out, tookMarker) {
		t.Fatalf("the helper exited without taking the claim, so this case "+
			"proves nothing about its release: %s", out)
	}
	claim, err := takeWithin(t, path)
	if err != nil {
		t.Fatalf("claim after the holder exited: %v — the claim outlived its "+
			"process and nothing can clear it", err)
	}
	claim.Release()
}

// --- the subprocess harness ------------------------------------------- //

// heldMarker is what the helper prints on a refusal. Its own string rather
// than matching ErrHeld's text, so a case fails when the two stop agreeing
// instead of quietly passing on a substring.
const heldMarker = "REFUSED-AS-HELD"

// tookMarker is what the helper prints once it holds the claim. A case that
// needs the helper to have held it requires the marker: an exit status of 0
// alone is also what a helper that never ran prints.
const tookMarker = "TOOK-THE-CLAIM"

// helperEnv names the sidecar the helper should claim, and helperModeEnv what
// it should do with the claim.
const (
	helperEnv     = "CREWLET_TEST_FILELOCK_PATH"
	helperModeEnv = "CREWLET_TEST_FILELOCK_MODE"
)

// runHelper runs the helper in mode against path and returns its output.
//
// Bounded by the same [hangDetector] as an in-process claim: a helper whose
// claim blocked on the holder would otherwise hold this case until the
// package's own timeout, and report nothing about why.
//
// With the race runtime's exit sleep switched off
// ([procgrouptest.StandInRaceOptions]): a helper that took the claim exits 0,
// and a race-built one would sleep a second on its way out with the case
// waiting on it. What the claim is held to — that the kernel frees it however
// the holder ends — does not depend on when the exit comes.
func runHelper(t *testing.T, path, mode string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), hangDetector)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=NoSuchTest") //nolint:gosec // os.Args[0] is this test binary
	cmd.Env = append(os.Environ(), procgrouptest.GORACE+"="+procgrouptest.StandInRaceOptions(),
		helperEnv+"="+path, helperModeEnv+"="+mode)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil && t.Context().Err() == nil {
		t.Fatalf("the helper's claim of %s has not answered in %v: it blocked on the "+
			"holder instead of saying who holds the path: %s", path, hangDetector, out)
	}
	return string(out), err
}

// runClaimHelper claims path and reports what happened.
func runClaimHelper(path, mode string) int {
	claim, err := filelock.Take(path)
	if err != nil {
		if errors.Is(err, filelock.ErrHeld) {
			fmt.Printf("%s %v\n", heldMarker, err)
			return 3
		}
		fmt.Printf("claim failed: %v\n", err)
		return 4
	}
	fmt.Println(tookMarker)
	if mode == "abandon" {
		// EXIT WITHOUT RELEASING, which is the crash this lock has to
		// survive. The kernel is what frees it.
		return 0
	}
	claim.Release()
	return 0
}
