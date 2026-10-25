package filelock_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/filelock"
)

// hangDetector bounds a claim this suite expects to come back at once. A
// claim is non-blocking by contract, so one that has not answered in this long
// has blocked — which is the failure, not a slow machine: the honest answer to
// a held path is "held, by whom", immediately.
const hangDetector = 30 * time.Second

// takeWithin takes path, failing the case if the claim blocks.
func takeWithin(t *testing.T, path string) (*filelock.Claim, error) {
	t.Helper()
	type answer struct {
		claim *filelock.Claim
		err   error
	}
	answered := make(chan answer, 1)
	go func() {
		claim, err := filelock.Take(path)
		answered <- answer{claim, err}
	}()
	select {
	case got := <-answered:
		return got.claim, got.err
	case <-time.After(hangDetector):
		t.Fatalf("a claim of %s has not answered in %v: it blocked on the holder "+
			"instead of saying who holds the path", path, hangDetector)
		return nil, nil
	}
}

// A SECOND CLAIM IS REFUSED, AT ONCE, NAMING THE PATH AND THE HOLDER.
//
// In one process, on two descriptors: flock belongs to an open file
// description, so a second open of the sidecar is a second claimant exactly as
// another process is. That is what makes this package's rule testable without
// a subprocess — and it is why the store's in-process SHARING of a claim is a
// refcount above this package rather than something flock does for it.
//
// Mutation: take the lock blocking (LOCK_EX without LOCK_NB) and this case
// hangs past its detector; drop the stamp and the holder names nobody.
func TestAClaimRefusesASecondClaimant(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "claim.lock")

	first, err := filelock.Take(path)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	defer first.Release()

	second, err := takeWithin(t, path)
	if err == nil {
		second.Release()
		t.Fatal("a second claim of a held path succeeded")
	}
	if !errors.Is(err, filelock.ErrHeld) {
		t.Fatalf("the refusal = %v; it must be errors.Is ErrHeld, or a caller cannot "+
			"tell a held path from a path that cannot be locked", err)
	}
	var held *filelock.HeldError
	if !errors.As(err, &held) {
		t.Fatalf("the refusal = %T; it must be a *HeldError carrying the holder", err)
	}
	if held.Path != path {
		t.Errorf("the refusal names %q, want the sidecar %q", held.Path, path)
	}
	if want := fmt.Sprintf("pid %d", os.Getpid()); !strings.Contains(held.Holder, want) {
		t.Errorf("the refusal's holder = %q; it must name the holding process (%s)", held.Holder, want)
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), held.Holder) {
		t.Errorf("the message %q must name both the path and its holder", err)
	}
}

// RELEASING FREES IT for the next claimant — a claim that outlived its release
// would make a clean restart impossible — and a release is safe twice, from
// two goroutines at once, and on a nil claim, so every caller can defer it
// without tracking whether it already ran.
func TestReleasingLetsTheNextClaimantIn(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "claim.lock")

	first, err := filelock.Take(path)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	var both sync.WaitGroup
	for range 2 {
		both.Go(first.Release)
	}
	both.Wait()
	first.Release()
	var nothing *filelock.Claim
	nothing.Release()

	second, err := takeWithin(t, path)
	if err != nil {
		t.Fatalf("the claim was not released: %v", err)
	}
	second.Release()
}

// THE SIDECAR SURVIVES A RELEASE. Unlinking it races a peer that has already
// opened it and is about to lock: that peer would lock an unlinked inode while
// a third created a new file and locked that, and both would believe they held
// the path.
func TestReleasingLeavesTheSidecarInPlace(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "claim.lock")

	claim, err := filelock.Take(path)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	claim.Release()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the sidecar was removed on release: %v — a peer mid-claim "+
			"would lock an inode nobody else can see", err)
	}
}

// THE STAMP NAMES THIS PROCESS, which is the whole of what a refused claimant
// has to go on.
func TestTheStampIdentifiesTheHolder(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "claim.lock")

	claim, err := filelock.Take(path)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer claim.Release()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	pid, ok := filelock.PID(strings.TrimSpace(string(raw)))
	if !ok {
		t.Fatalf("the stamp carries no pid: %q", raw)
	}
	if pid != os.Getpid() {
		t.Fatalf("stamp pid = %d, want this process (%d)", pid, os.Getpid())
	}
}

// A PATH THAT CANNOT HOLD A CLAIM IS NOT A HELD ONE. A sidecar whose directory
// does not exist has no holder to name, and a caller answers the two in
// different words — "stop the process that has it" against "this path is
// wrong" — so the failure is wrapped, names the path, and is never ErrHeld.
func TestAPathThatCannotHoldAClaimIsNotAHeldOne(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing", "claim.lock")

	claim, err := filelock.Take(path)
	if err == nil {
		claim.Release()
		t.Fatal("a sidecar in a directory that does not exist was claimed")
	}
	if errors.Is(err, filelock.ErrHeld) {
		t.Fatalf("the failure = %v; a path that cannot hold a claim was reported as held", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the failure = %v; it must wrap the cause", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the failure = %v; it must name the path", err)
	}
}

// PID READS A STAMP AND NOTHING ELSE: a line that is not one has no pid, and
// an invented one would send an operator to the wrong process.
func TestPIDReadsOnlyAStamp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		stamp string
		pid   int
		ok    bool
	}{
		{"pid 41 on host since 2026-10-11T00:00:00Z", 41, true},
		{"pid 41", 41, true},
		{"", 0, false},
		{"another crewlet process", 0, false},
		{"pid forty-one on host", 0, false},
		{"PID 41 on host", 0, false},
	} {
		pid, ok := filelock.PID(tc.stamp)
		if pid != tc.pid || ok != tc.ok {
			t.Errorf("PID(%q) = %d, %v; want %d, %v", tc.stamp, pid, ok, tc.pid, tc.ok)
		}
	}
}
