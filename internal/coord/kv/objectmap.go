package kv

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the object placement map ------------------------------------------ //

// ObjectMap reads the map.
func (f *FleetStore) ObjectMap(ctx context.Context) (coord.ObjectMapRecord, bool, error) {
	entry, err := f.objects.Get(ctx, objectMapKey)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.ObjectMapRecord{}, false, nil
	case err != nil:
		return coord.ObjectMapRecord{}, false, unavailable("read the object placement map", err)
	}
	return coord.ObjectMapRecord{Value: entry.Value(), Version: entry.Revision()}, true, nil
}

// CreateObjectMap writes the first map, leaving an existing one alone.
//
// Create rather than Put, so two duty holders racing on an empty fleet write
// one map between them and the loser updates the winner's.
func (f *FleetStore) CreateObjectMap(ctx context.Context, value []byte) (coord.ObjectMapRecord, bool, error) {
	if len(value) == 0 {
		return coord.ObjectMapRecord{}, false, errors.New("coord/kv: an object map needs a value")
	}
	revision, err := f.objects.Create(ctx, objectMapKey, value)
	switch {
	case errors.Is(err, jetstream.ErrKeyExists):
		return coord.ObjectMapRecord{}, false, nil
	case err != nil:
		return coord.ObjectMapRecord{}, false, unavailable("create the object placement map", err)
	}
	return coord.ObjectMapRecord{Value: value, Version: revision}, true, nil
}

// UpdateObjectMap writes the map at the version it was read at.
func (f *FleetStore) UpdateObjectMap(ctx context.Context, value []byte, version uint64) (coord.ObjectMapRecord, bool, error) {
	if len(value) == 0 {
		return coord.ObjectMapRecord{}, false, errors.New("coord/kv: an object map needs a value")
	}
	if version == 0 {
		// NO VERSION IS A LOST RACE, never an unconditional write: the
		// client reads an expected revision of 0 as "must not exist yet".
		return coord.ObjectMapRecord{}, false, nil
	}
	revision, err := f.objects.Update(ctx, objectMapKey, value, version)
	switch {
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.ObjectMapRecord{}, false, nil
	case err != nil:
		return coord.ObjectMapRecord{}, false, unavailable("update the object placement map", err)
	}
	return coord.ObjectMapRecord{Value: value, Version: revision}, true, nil
}
