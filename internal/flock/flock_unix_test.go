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
// flock belongs to the open file description, not a descriptor: a forked, not
// yet exec'd child holds a duplicate, so closing the lock's own descriptor
// leaves it held while any copy lives; only LOCK_UN drops it. The dup kept
// open here is that fork's window made deterministic (close-on-exec, so
// parallel cases' helpers never inherit it). Internal, because a fork cannot
// be held open at a chosen instant from outside.
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
