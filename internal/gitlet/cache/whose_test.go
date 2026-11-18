package cache_test

import (
	"context"
	"errors"
	"io/fs"
	"syscall"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SYNC'S FAILURE SAYS WHOSE IT IS, by what failed, never by elimination:
// the estate failing to answer a page is the code log's (ErrLogRead); the
// object store failing to hand a pack over, at its open or mid-read, for any
// reason but an answer about the pack, is the store's (ErrStoreRead), never a
// pack declared missing; this node's disk refusing a staged write is neither;
// and the log's answer that no row holds the repository is no read failure.
// Each gets its own sentence from the host, and only the code log's says the
// node is cut off from the fleet: misattributed, a store throttle sends a
// person to the quorum, and a full volume to the bucket.
//
// Mutations: drop logRead's or storeFailure's wrap, and that sentinel is lost;
// mark the whole copy's failure the store's (download), and the disk's is.
func TestASyncFailureSaysWhoseItIs(t *testing.T) {
	t.Parallel()
	linearizable := statelog.Freshness{Level: statelog.ReadLinearizable}
	t.Run("the code log", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		c := w.open(t)
		noQuorum := errors.New("the barrier found no quorum")
		w.log.set(func(f *fakeLog) { f.hook = func(fakeCall) error { return noQuorum } })
		_, err := c.Sync(t.Context(), repo, linearizable)
		if !errors.Is(err, cache.ErrLogRead) || !errors.Is(err, noQuorum) || errors.Is(err, cache.ErrStoreRead) {
			t.Errorf("a sync the estate could not answer = %v, want ErrLogRead wrapping the estate's error", err)
		}
	})
	t.Run("the object store", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		throttled := errors.New("SlowDown: please reduce your request rate")
		w.objects.mu.Lock()
		w.objects.fail = func(context.Context, objstore.Key) error { return throttled }
		w.objects.mu.Unlock()
		c := w.open(t)
		_, err := c.Sync(t.Context(), repo, linearizable)
		var missing *cache.ErrPackMissing
		if !errors.Is(err, cache.ErrStoreRead) || !errors.Is(err, throttled) || errors.Is(err, cache.ErrLogRead) ||
			errors.As(err, &missing) {
			t.Errorf("a sync whose pack the store did not hand over = %v, want ErrStoreRead wrapping the "+
				"store's error, and no pack declared missing", err)
		}
	})
	t.Run("the object store, mid-read", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		reset := errors.New("read tcp: connection reset by peer")
		w.objects.mu.Lock()
		w.objects.readFail = func(objstore.Key) error { return reset }
		w.objects.mu.Unlock()
		c := w.open(t)
		_, err := c.Sync(t.Context(), repo, linearizable)
		if !errors.Is(err, cache.ErrStoreRead) || !errors.Is(err, reset) || errors.Is(err, cache.ErrLogRead) {
			t.Errorf("a sync whose pack's read the store broke off = %v, want ErrStoreRead wrapping its error", err)
		}
	})
	t.Run("this node's disk", func(t *testing.T) {
		// A full or failing cache volume refusing the round's staged write is
		// this node's, never the object store's.
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		full := &fs.PathError{Op: "write", Path: "tmp/pack-1.pack", Err: syscall.ENOSPC}
		c := w.open(t, cache.StagedWriteFails(func(objstore.Key) error { return full }))
		_, err := c.Sync(t.Context(), repo, linearizable)
		var missing *cache.ErrPackMissing
		if !errors.Is(err, syscall.ENOSPC) || errors.Is(err, cache.ErrStoreRead) || errors.Is(err, cache.ErrLogRead) ||
			errors.As(err, &missing) {
			t.Errorf("a sync whose staged file the disk refused = %v, want the disk's error, neither the store's "+
				"nor the code log's", err)
		}
	})
	t.Run("an answer", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		c := w.open(t)
		_, err := c.Sync(t.Context(), uuid.NewString(), linearizable)
		if !errors.Is(err, gitlet.ErrRepoNotFound) || errors.Is(err, cache.ErrLogRead) {
			t.Errorf("a sync of a repository no row holds = %v, want ErrRepoNotFound and no ErrLogRead", err)
		}
	})
}
