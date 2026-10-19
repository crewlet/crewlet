package hostbox

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ONE LOCK FILE, BESIDE THE DIRECTORY: every writer of a shared credential
// directory derives the same name from it, and the name is outside it, so a
// copy of the directory never carries the lock.
func TestTheCredentialLockSitsBesideItsDirectory(t *testing.T) {
	t.Parallel()
	for dir, want := range map[string]string{
		"/state/credentials":   "/state/credentials.lock",
		"/state/credentials/":  "/state/credentials.lock",
		"/state/./credentials": "/state/credentials.lock",
	} {
		if got := CredentialLock(dir); got != want {
			t.Errorf("CredentialLock(%q) = %q, want %q", dir, got, want)
		}
	}
}

// TWO TAKERS IN ONE PROCESS EXCLUDE EACH OTHER, as two processes do: a sandbox
// teardown and a seat in the same engine take the lock through descriptors of
// their own, and an flock belongs to the open file description. A taker that
// cannot have it within the wait is told so as a timeout, and the lock is
// free again the moment its holder lets go.
//
// Mutation: share one descriptor between takers, or answer a held lock as
// taken, and the second taker gets the lock while the first holds it.
func TestACredentialLockExcludesAnotherTakerUntilItIsReleased(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "credentials.lock")
	release, err := LockFile(t.Context(), path, time.Second, "the case itself")
	if err != nil {
		t.Fatalf("taking a free lock: %v", err)
	}

	start := time.Now()
	_, err = LockFile(t.Context(), path, 100*time.Millisecond, "the case's first taker")
	var held *LockWaitError
	if !errors.As(err, &held) || held.Path != path {
		t.Fatalf("a second taker of a held lock got %v, want a LockWaitError naming %s", err, path)
	}
	if !strings.Contains(err.Error(), "the case's first taker") {
		t.Errorf("the refusal %q does not name who the caller said holds the lock", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a lock held past the wait is not a timeout: %v", err)
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Errorf("the second taker gave up after %s, before its wait", waited)
	}
	// A taker that will not wait is told at once that the lock is busy.
	if _, err := LockFile(t.Context(), path, 0, "the case"); !errors.As(err, &held) {
		t.Fatalf("a taker that waits for nothing got %v, want a LockWaitError", err)
	}

	// Released while a third taker waits: it gets the lock rather than the
	// timeout.
	got := make(chan error, 1)
	go func() {
		again, err := LockFile(context.Background(), path, 5*time.Second, "the case")
		if err == nil {
			again()
		}
		got <- err
	}()
	time.Sleep(50 * time.Millisecond)
	release()
	if err := <-got; err != nil {
		t.Fatalf("a taker waiting on a lock its holder released: %v", err)
	}
}

// A waiter whose caller went away stops waiting, saying why.
func TestACredentialLockWaitEndsWithItsContext(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "credentials.lock")
	release, err := LockFile(t.Context(), path, time.Second, "the case itself")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := LockFile(ctx, path, 10*time.Second, "the case"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled waiter got %v, want context.Canceled", err)
	}
}
