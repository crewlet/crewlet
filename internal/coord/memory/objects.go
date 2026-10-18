package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
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

// MoveObjectBackend replaces the recorded backend from with to, conditional
// on from being recorded.
func (f *Fleet) MoveObjectBackend(_ context.Context, from, to string) error {
	if from == "" || to == "" {
		return errors.New("coord/memory: a move of the object backend names both stores")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.objectBackend {
	case to:
		return nil
	case from:
		f.objectBackend = to
		return nil
	case "":
		return fmt.Errorf("%w: no backend is recorded", coord.ErrObjectBackendMoved)
	}
	return fmt.Errorf("%w: %q is recorded", coord.ErrObjectBackendMoved, f.objectBackend)
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
