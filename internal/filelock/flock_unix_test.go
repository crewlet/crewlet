//go:build linux || darwin

package filelock

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// UNLOCK BEFORE CLOSE: a release frees the path even while ANOTHER DESCRIPTOR
// for the claim's open file description is still open.
//
// flock belongs to the open file description, not to a descriptor, and every
// duplicate of the claim's descriptor refers to that same description — a
// child this process forked and has not yet exec'd holds one (a node that
// runs git processes forks constantly), and so does anything that dup'd it.
// Closing the claim's own descriptor leaves the lock held for as long as any
// such copy lives; only LOCK_UN drops it whoever else has the description
// open. The copy here is a dup kept open across the release, which is that
// fork's window made deterministic. Close-on-exec, so the helpers this binary
// execs in parallel cases never inherit it.
//
// Internal, because the descriptor is the claim's own and nothing outside the
// package can reach it — and a fork cannot be held open at a chosen instant
// from a test.
//
// Mutation: make Release close without unlocking and the copy keeps the lock,
// so the next claim of the path is refused as held.
func TestReleaseFreesThePathWhileACopyOfItsDescriptorLives(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "claim.lock")

	claim, err := Take(path)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	held := claim.file.Load()
	copied, err := unix.FcntlInt(held.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		claim.Release()
		t.Fatalf("duplicating the claim's descriptor: %v", err)
	}
	defer func() { _ = unix.Close(copied) }()

	claim.Release()

	next, err := Take(path)
	if err != nil {
		t.Fatalf("the path is still held after its claim was released, by a "+
			"descriptor that outlived the release: %v — a release that only "+
			"closes leaves the lock with every copy of the description", err)
	}
	next.Release()
}
