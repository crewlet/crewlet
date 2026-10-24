package memory

import (
	"context"
	"errors"
	"slices"
)

// ---- the trash reaper ------------------------------------------------- //

// RecordTrashReap stores the trash reaper's last report, replacing the one
// before.
func (f *Fleet) RecordTrashReap(_ context.Context, value []byte) error {
	if len(value) == 0 {
		return errors.New("coord/memory: a trash reap report needs a value")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trashReap = slices.Clone(value)
	return nil
}

// TrashReap reads the trash reaper's last report.
func (f *Fleet) TrashReap(context.Context) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.trashReap == nil {
		return nil, false, nil
	}
	return slices.Clone(f.trashReap), true, nil
}
