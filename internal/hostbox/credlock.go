package hostbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// CredentialLockWait bounds how long a writer of a shared login waits for
// another writer to finish with it.
//
// What the lock guards is one compare-and-swap of a login file — a read and a
// rename of a few kilobytes, milliseconds even on a loaded disk — so ten
// seconds is three orders of magnitude of headroom. Only a holder that has
// STOPPED RUNNING reaches it (a `crewlet llm` command suspended at a terminal
// mid-write), and then the honest answer is an error naming the lock rather
// than a teardown or a seat's release that never returns. A holder that
// crashed holds nothing: the lock is its open descriptor's, and the kernel
// drops it with the process.
const CredentialLockWait = 10 * time.Second

// credentialLockPoll is how often a writer waiting on another's lock asks
// again. A lock held for milliseconds is free within a poll or two, and a
// waiter sleeping between them costs nothing.
const credentialLockPoll = 10 * time.Millisecond

// CredentialLock is the lock file of the shared credential directory dir:
// dir + ".lock", BESIDE the directory rather than in it, so that nothing which
// copies the directory — a login bundle, a box's seed — ever carries the lock
// along with the logins.
//
// ONE NAME FOR EVERY WRITER. The CLI LLM backend's seats and `crewlet llm`
// commands write the shared directory, and so does every local sandbox's
// teardown, writing back a login its run refreshed: a compare-and-swap is one
// only against writers that agree on the lock, and each deriving the name for
// itself is how one of them comes to lock a file nobody else does.
func CredentialLock(dir string) string { return filepath.Clean(dir) + ".lock" }

// LockCredentials takes the shared credential directory dir's lock
// ([CredentialLock]) for one compare-and-swap of a login in it, waiting up to
// [CredentialLockWait] for another writer to let go, and hands back its
// release. A lock still held at the end of the wait is a [*LockWaitError].
func LockCredentials(ctx context.Context, dir string) (func(), error) {
	return LockFile(ctx, CredentialLock(dir), CredentialLockWait)
}

// LockWaitError is a lock another holder kept for longer than the wait: a
// TIMEOUT ([context.DeadlineExceeded]), so a caller that may move on past a
// slow dependency reads it as one, and one that waited zero reads it as the
// lock being busy.
type LockWaitError struct {
	// Path is the lock file.
	Path string
	// Wait is how long the caller waited for it.
	Wait time.Duration
}

func (e *LockWaitError) Error() string {
	return fmt.Sprintf("hostbox: the lock %s was still held by another writer after %s — a "+
		"holder that stopped running (a `crewlet llm` command suspended at a terminal) keeps "+
		"it until it exits", e.Path, e.Wait)
}

func (e *LockWaitError) Unwrap() error { return context.DeadlineExceeded }

// LockFile takes an exclusive flock on path, creating it at [FileMode],
// waiting up to wait — zero for not at all — for another holder to let go, and
// hands back its release. A lock still held at the end of the wait is a
// [*LockWaitError].
//
// An flock rather than an fcntl lock, for the reason internal/store gives: an
// fcntl lock is dropped when ANY descriptor for the file is closed in the
// process. And a fresh descriptor per call, because an flock belongs to the
// open file description — two takers in one process exclude each other
// exactly as two processes do, which is what lets a sandbox teardown and a
// seat in the same engine share the shared login's lock.
func LockFile(ctx context.Context, path string, wait time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), DirMode); err != nil {
		return nil, fmt.Errorf("hostbox: preparing %q: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, FileMode) //nolint:gosec // the caller's own state directory
	if err != nil {
		return nil, fmt.Errorf("hostbox: opening the lock %q: %w", path, err)
	}
	fd := int(f.Fd()) //nolint:gosec // a descriptor fits an int on every platform this builds for
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(fd, syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("hostbox: locking %q: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, &LockWaitError{Path: path, Wait: wait}
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("hostbox: waiting for the lock %q: %w", path, ctx.Err())
		case <-time.After(credentialLockPoll):
		}
	}
}
