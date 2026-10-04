package memory

import (
	"context"
	"errors"
	"slices"
	"time"
)

// ---- the object store ------------------------------------------------- //

// chunkLock is one held chunk lock.
type chunkLock struct {
	owner string
	at    time.Time
}

// SetChunkLockTTL ages chunk locks at d rather than coord.ChunkLockTTL — the
// age the KV backend's bucket is created with — so a test can watch a dead
// holder's lock lapse without waiting a minute.
func (f *Fleet) SetChunkLockTTL(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chunkLockTTL = d
}

// AgreeObjectBackend records identity unless a backend is recorded, and
// answers the recorded one.
func (f *Fleet) AgreeObjectBackend(_ context.Context, identity string) (string, error) {
	if identity == "" {
		return "", errors.New("coord/memory: an object backend needs an identity")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objectBackend == "" {
		f.objectBackend = identity
	}
	return f.objectBackend, nil
}

// LockChunk takes chunk's lock for owner unless another owner holds it.
func (f *Fleet) LockChunk(_ context.Context, chunk, owner string) (bool, error) {
	if chunk == "" || owner == "" {
		return false, errors.New("coord/memory: a chunk lock needs a chunk and an owner")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	if held, ok := f.chunkLocks[chunk]; ok && now.Sub(held.at) < f.chunkLockTTL && held.owner != owner {
		return false, nil
	}
	f.chunkLocks[chunk] = chunkLock{owner: owner, at: now}
	return true, nil
}

// UnlockChunk lets go of chunk's lock if owner holds it.
func (f *Fleet) UnlockChunk(_ context.Context, chunk, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if held, ok := f.chunkLocks[chunk]; ok && held.owner == owner {
		delete(f.chunkLocks, chunk)
	}
	return nil
}

// RecordObjectCollection stores the collector's last report.
func (f *Fleet) RecordObjectCollection(_ context.Context, value []byte) error {
	if len(value) == 0 {
		return errors.New("coord/memory: a collection report needs a value")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objectCollection = slices.Clone(value)
	return nil
}

// ObjectCollection reads the collector's last report.
func (f *Fleet) ObjectCollection(context.Context) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objectCollection == nil {
		return nil, false, nil
	}
	return slices.Clone(f.objectCollection), true, nil
}
