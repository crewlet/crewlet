package hostbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/crewlet/crewlet/internal/flock"
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
// ONE NAME FOR EVERY WRITER. The harness backend's seats and `crewlet llm`
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
	return LockFile(ctx, CredentialLock(dir), CredentialLockWait, credentialHolder)
}

// credentialHolder is who keeps the shared credential directory's lock for
// longer than [CredentialLockWait]: every writer holds it for milliseconds, so
// only one that has stopped running mid-write.
const credentialHolder = "another writer of the shared login — a `crewlet llm` command " +
	"stopped part way through (a suspended terminal) holds it until it exits"

// LockWaitError is a lock another holder kept for longer than the wait: a
// TIMEOUT ([context.DeadlineExceeded]), so a caller that may move on past a
// slow dependency reads it as one, and one that waited zero reads it as the
// lock being busy.
type LockWaitError struct {
	// Path is the lock file.
	Path string
	// Wait is how long the caller waited for it.
	Wait time.Duration
	// Holder says who keeps the lock that long, as the caller that knows what
	// the lock guards put it: what the reader has to go and find.
	Holder string
}

func (e *LockWaitError) Error() string {
	return fmt.Sprintf("hostbox: the lock %s was held for longer than %s by %s", e.Path, e.Wait, e.Holder)
}

func (e *LockWaitError) Unwrap() error { return context.DeadlineExceeded }

// LockFile takes an exclusive lock on path ([flock.Try]), waiting up to wait —
// zero for not at all — for another holder to let go, and hands back its
// release. A lock still held at the end of the wait is a [*LockWaitError]
// naming holder: who keeps this lock that long is the caller's knowledge, not
// this function's.
//
// A fresh lock per call, because the lock belongs to the open file description
// — two takers in one process exclude each other exactly as two processes do,
// which is what lets a sandbox teardown and a seat in the same engine share
// the shared login's lock.
func LockFile(ctx context.Context, path string, wait time.Duration, holder string) (func(), error) {
	deadline := time.Now().Add(wait)
	for {
		lock, err := flock.Try(path)
		if err == nil {
			return lock.Release, nil
		}
		if !errors.Is(err, flock.ErrHeld) {
			return nil, fmt.Errorf("hostbox: %w", err)
		}
		if !time.Now().Before(deadline) {
			return nil, &LockWaitError{Path: path, Wait: wait, Holder: holder}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("hostbox: waiting for the lock %q: %w", path, ctx.Err())
		case <-time.After(credentialLockPoll):
		}
	}
}
