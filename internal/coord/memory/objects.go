package memory

import (
	"context"
	"errors"
	"slices"
)

// ---- the object store ------------------------------------------------- //

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

// RetireChunkLocks has nothing to retire: the twin lives and dies with its
// process, so no earlier build's chunk locks can exist in it.
func (f *Fleet) RetireChunkLocks(context.Context) (bool, error) {
	return false, nil
}
