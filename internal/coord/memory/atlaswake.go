package memory

import (
	"bytes"
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the atlas wakes of owning seats ----------------------------------- //

// AtlasWakes is every record, by its key.
func (f *Fleet) AtlasWakes(context.Context) (map[string]coord.AtlasWake, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]coord.AtlasWake, len(f.atlasWakes))
	for key, r := range f.atlasWakes {
		out[key] = coord.AtlasWake{Value: bytes.Clone(r.Value), Revision: r.Revision}
	}
	return out, nil
}

// AtlasWake is one seat's record on one entry and its revision, false where none.
func (f *Fleet) AtlasWake(_ context.Context, handle, entry string) ([]byte, uint64, bool, error) {
	if handle == "" || entry == "" {
		return nil, 0, false, errors.New("coord/memory: an atlas wake is read per seat and entry")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.atlasWakes[coord.AtlasWakeKey(handle, entry)]
	return bytes.Clone(r.Value), r.Revision, ok, nil
}

// CreateAtlasWake writes a first record, false where one exists.
func (f *Fleet) CreateAtlasWake(_ context.Context, handle, entry string, value []byte) (bool, error) {
	if handle == "" || entry == "" {
		return false, errors.New("coord/memory: an atlas wake names its seat and its entry")
	}
	key := coord.AtlasWakeKey(handle, entry)
	if err := coord.HoldValue(key, len(value)); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.atlasWakes[key]; ok {
		return false, nil
	}
	f.atlasWakeRevision++
	f.atlasWakes[key] = coord.AtlasWake{Value: bytes.Clone(value), Revision: f.atlasWakeRevision}
	return true, nil
}

// UpdateAtlasWake replaces the record read at revision, false where it moved.
func (f *Fleet) UpdateAtlasWake(_ context.Context, handle, entry string, value []byte, revision uint64) (bool, error) {
	if handle == "" || entry == "" || revision == 0 {
		return false, errors.New("coord/memory: an atlas wake is updated at the revision it was read at")
	}
	key := coord.AtlasWakeKey(handle, entry)
	if err := coord.HoldValue(key, len(value)); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.atlasWakes[key]; !ok || r.Revision != revision {
		return false, nil
	}
	f.atlasWakeRevision++
	f.atlasWakes[key] = coord.AtlasWake{Value: bytes.Clone(value), Revision: f.atlasWakeRevision}
	return true, nil
}

// DeleteAtlasWake drops the record read at revision, false where it moved.
func (f *Fleet) DeleteAtlasWake(_ context.Context, handle, entry string, revision uint64) (bool, error) {
	if handle == "" || entry == "" || revision == 0 {
		return false, errors.New("coord/memory: an atlas wake is deleted at the revision it was read at")
	}
	key := coord.AtlasWakeKey(handle, entry)
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.atlasWakes[key]; !ok || r.Revision != revision {
		return false, nil
	}
	delete(f.atlasWakes, key)
	return true, nil
}
