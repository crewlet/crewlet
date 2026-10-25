//go:build linux || darwin

package flock

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// UNLOCK BEFORE CLOSE: a release frees the lock even while ANOTHER DESCRIPTOR
// for its open file description is still open.
//
// flock belongs to the open file description, not to a descriptor, and every
// duplicate of the lock's descriptor refers to that same description — a child
// this process forked and has not yet exec'd holds one (a node that runs git
// and coding CLIs forks constantly), and so does anything that dup'd it.
// Closing the lock's own descriptor leaves the lock held for as long as any
// such copy lives; only LOCK_UN drops it whoever else has the description
// open. The copy here is a dup kept open across the release, which is that
// fork's window made deterministic. Close-on-exec, so the helpers this binary
// execs in parallel cases never inherit it.
//
// Internal, because the descriptor is the lock's own and nothing outside the
// package can reach it — and a fork cannot be held open at a chosen instant
// from a test.
//
// Mutation: make Release close without unlocking and the copy keeps the lock,
// so the next Try of the path is refused as held.
func TestReleaseFreesTheLockWhileACopyOfItsDescriptorLives(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x.lock")

	lock, err := Try(path)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	held := lock.file.Load()
	copied, err := unix.FcntlInt(held.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		lock.Release()
		t.Fatalf("duplicating the lock's descriptor: %v", err)
	}
	defer func() { _ = unix.Close(copied) }()

	lock.Release()

	next, err := Try(path)
	if err != nil {
		t.Fatalf("the lock is still held after its release, by a descriptor "+
			"that outlived the release: %v — a release that only closes "+
			"leaves the lock with every copy of the description", err)
	}
	next.Release()
}
