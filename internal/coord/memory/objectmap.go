package memory

import (
	"context"
	"errors"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the object placement map ------------------------------------------ //

// ObjectMap reads the map.
func (f *Fleet) ObjectMap(context.Context) (coord.ObjectMapRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objectMap == nil {
		return coord.ObjectMapRecord{}, false, nil
	}
	return coord.ObjectMapRecord{Value: slices.Clone(f.objectMap.Value),
		Version: f.objectMap.Version}, true, nil
}

// CreateObjectMap writes the first map, leaving an existing one alone.
func (f *Fleet) CreateObjectMap(_ context.Context, value []byte) (coord.ObjectMapRecord, bool, error) {
	if len(value) == 0 {
		return coord.ObjectMapRecord{}, false, errors.New("coord/memory: an object map needs a value")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objectMap != nil {
		return coord.ObjectMapRecord{}, false, nil
	}
	return f.storeObjectMapLocked(value), true, nil
}

// UpdateObjectMap writes the map at the version it was read at.
func (f *Fleet) UpdateObjectMap(_ context.Context, value []byte, version uint64) (coord.ObjectMapRecord, bool, error) {
	if len(value) == 0 {
		return coord.ObjectMapRecord{}, false, errors.New("coord/memory: an object map needs a value")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objectMap == nil || f.objectMap.Version != version {
		return coord.ObjectMapRecord{}, false, nil
	}
	return f.storeObjectMapLocked(value), true, nil
}

// storeObjectMapLocked writes the map under a fresh version, drawn from the
// same sequence every other record here takes its versions from.
func (f *Fleet) storeObjectMapLocked(value []byte) coord.ObjectMapRecord {
	f.version++
	f.objectMap = &coord.ObjectMapRecord{Value: slices.Clone(value), Version: f.version}
	return coord.ObjectMapRecord{Value: slices.Clone(value), Version: f.version}
}
