package nativelib

import (
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

// A LOCK IS NOT STOLEN WHILE IT IS FRESH: the whole point is that a peer
// mid-extraction is waited for rather than raced.
func TestAFreshLockIsWaitedForRatherThanStolen(t *testing.T) {
	dir := t.TempDir()
	first, held := Lock(dir, time.Now().Add(time.Minute))
	if !held {
		t.Fatal("the first caller did not take the lock")
	}
	defer first()

	start := time.Now()
	if _, held := Lock(dir, time.Now().Add(150*time.Millisecond)); held {
		t.Fatal("a second caller took a lock the first still holds")
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("the second caller gave up after %v; it must wait for the peer that is "+
			"extracting, or the serialisation buys nothing", waited)
	}
}

// AND A STALE ONE IS RECLAIMED. A lock is held only across an extraction, so
// one older than that belongs to a process that was killed holding it,
// and never reclaiming it would wedge every engine on the host.
func TestAStaleLockIsReclaimed(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, lockName)
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	release, held := Lock(dir, time.Now().Add(time.Second))
	if !held {
		t.Fatal("an abandoned lock was never reclaimed")
	}
	release()
}

// DEBRIS AT THE LOCK PATH IS CLEARED RATHER THAN WAITED FOR. Only this code
// creates that path and it only ever creates a directory, so a file there is
// not a peer, and treating it as one would stall every engine on the host for
// the whole timeout, on this start and on every start after it.
func TestDebrisAtTheLockPathIsCleared(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	release, held := Lock(dir, time.Now().Add(time.Minute))
	if !held {
		t.Fatal("a file at the lock path was treated as a lock somebody holds")
	}
	release()
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("clearing debris took %v; it must not wait out the timeout", waited)
	}
}

// A LOCK THAT CANNOT BE TAKEN AT ALL IS NOT AN ERROR, and is not waited for
// either: there is no peer to wait for when the path cannot hold a lock.
// the caller runs its extraction unguarded in that case, which is
// the behaviour before any of this existed and strictly better than an engine
// that refuses to start.
func TestALockThatCannotBeTakenIsGivenUpAtOnce(t *testing.T) {
	notADirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADirectory, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, held := Lock(notADirectory, time.Now().Add(time.Minute)); held {
		t.Fatal("Lock claimed a lock it does not hold")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("gave up after %v; a path that cannot hold a lock has no peer to wait for", waited)
	}
}

// A HELD LOCK IS RENEWED FOR AS LONG AS ITS HOLDER WORKS, so a download that
// outlasts lockStale is not raced by a peer that read its lock as abandoned.
// In a bubble, on the bubble's clock: the lock's time is set to it, the holder
// works three stale periods, and a peer still waits. Mutation: drop the
// renewal, and the peer reclaims the lock.
func TestAHeldLockIsRenewedWhileItsHolderWorks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		release, held := Lock(dir, time.Now().Add(time.Minute))
		if !held {
			t.Fatal("the first caller did not take the lock")
		}
		now := time.Now()
		if err := os.Chtimes(filepath.Join(dir, lockName), now, now); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * lockStale)
		if _, held := Lock(dir, time.Now().Add(lockStale/2)); held {
			t.Error("a peer took a lock its holder was still renewing")
		}
		release()
	})
}
