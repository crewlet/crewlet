package kv

import (
	"bytes"
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the atlas wakes of owning seats ----------------------------------- //

// AtlasWakes is every record, by its key.
func (f *FleetStore) AtlasWakes(ctx context.Context) (map[string]coord.AtlasWake, error) {
	out := map[string]coord.AtlasWake{}
	err := f.eachUnder(ctx, f.positions, coord.AtlasWakeFilter(), "the atlas wakes",
		func(kve jetstream.KeyValueEntry) error {
			if _, _, ok := coord.AtlasWakeOf(kve.Key()); ok {
				out[kve.Key()] = coord.AtlasWake{Value: bytes.Clone(kve.Value()), Revision: kve.Revision()}
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AtlasWake is one seat's record on one entry and its revision, false where none.
func (f *FleetStore) AtlasWake(ctx context.Context, handle, entry string) ([]byte, uint64, bool, error) {
	if handle == "" || entry == "" {
		return nil, 0, false, errors.New("coord/kv: an atlas wake is read per seat and entry")
	}
	kve, err := f.get(ctx, f.positions, coord.AtlasWakeKey(handle, entry))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil, 0, false, nil
	case err != nil:
		return nil, 0, false, unavailable("read "+handle+"'s atlas wake", err)
	}
	return bytes.Clone(kve.Value()), kve.Revision(), true, nil
}

// CreateAtlasWake writes a first record, false where one exists.
func (f *FleetStore) CreateAtlasWake(ctx context.Context, handle, entry string, value []byte) (bool, error) {
	if handle == "" || entry == "" {
		return false, errors.New("coord/kv: an atlas wake names its seat and its entry")
	}
	_, err := f.create(ctx, f.positions, coord.AtlasWakeKey(handle, entry), value)
	switch {
	case errors.Is(err, jetstream.ErrKeyExists):
		return false, nil
	case err != nil:
		return false, unavailable("create "+handle+"'s atlas wake", err)
	}
	return true, nil
}

// UpdateAtlasWake replaces the record read at revision, false where it moved.
func (f *FleetStore) UpdateAtlasWake(ctx context.Context, handle, entry string, value []byte,
	revision uint64) (bool, error) {

	if handle == "" || entry == "" || revision == 0 {
		return false, errors.New("coord/kv: an atlas wake is updated at the revision it was read at")
	}
	_, err := f.positions.Update(ctx, coord.AtlasWakeKey(handle, entry), value, revision)
	switch {
	case err != nil && lostUpdateRace(err):
		return false, nil
	case err != nil:
		return false, unavailable("update "+handle+"'s atlas wake", err)
	}
	return true, nil
}

// DeleteAtlasWake purges the record read at revision, false where it moved.
func (f *FleetStore) DeleteAtlasWake(ctx context.Context, handle, entry string, revision uint64) (bool, error) {
	if handle == "" || entry == "" || revision == 0 {
		return false, errors.New("coord/kv: an atlas wake is deleted at the revision it was read at")
	}
	err := f.positions.Purge(ctx, coord.AtlasWakeKey(handle, entry), jetstream.LastRevision(revision))
	switch {
	case err == nil:
		return true, nil
	case lostUpdateRace(err):
		return false, nil
	}
	return false, unavailable("delete "+handle+"'s atlas wake", err)
}
