package cache_test

import (
	"errors"
	"io/fs"
	"syscall"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SYNC'S FAILURE SAYS WHOSE IT IS, by what failed and never by
// elimination. The estate failing to answer a page is the code log's
// (ErrLogRead); the object store failing to hand a pack over — at its open or
// part way through its read — for no reason that is an answer about the pack
// is the store's (ErrStoreRead), never a pack declared missing; this node's
// own disk refusing a write into the download's staged files is neither; and
// the log's ANSWER that no row holds the repository is that answer, not a
// failure to read it. The host answers each with its own sentence, and only
// the code log's tells a person the node is cut off from the fleet — which,
// given to a store throttle, sent them to the fleet's quorum, and the store's,
// given to a full volume, sent them to the bucket.
//
// Mutations: drop logRead's wrap, and the code log's failure is no
// ErrLogRead; drop storeFailure's, and the store's is no ErrStoreRead; mark
// the whole copy's failure as the store's (download), and the disk's is.
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
		w.objects.fail = func(objstore.Key) error { return throttled }
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
		// The download's other side: a write into the round's own staged
		// files failing — a full or failing cache volume — is this node's,
		// never the object store's.
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
		if !errors.Is(err, code.ErrRepoNotFound) || errors.Is(err, cache.ErrLogRead) {
			t.Errorf("a sync of a repository no row holds = %v, want ErrRepoNotFound and no ErrLogRead", err)
		}
	})
}
