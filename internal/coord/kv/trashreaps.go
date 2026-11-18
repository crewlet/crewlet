package kv

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"
)

// ---- the trash reaper ------------------------------------------------- //

// trashReapKey is the trash reaper's last report, kept in the objects bucket
// beside the object collector's ([collectionKey]): the same kind of record, a
// duty's whole account of its last pass, read by the next holder. Both need
// that bucket's NO AGE: an expired report reads as a duty that never ran, a
// pass due at once rather than when the last holder scheduled it. A bucket of
// its own would only add a stream every node provisions at boot.
const trashReapKey = "trash-reap"

// RecordTrashReap stores the trash reaper's last report, replacing the one
// before: one writer at a time holds the duty, and a report is a whole account
// rather than a delta, so the last write is the answer ([coord.TrashReaps]).
func (f *FleetStore) RecordTrashReap(ctx context.Context, value []byte) error {
	if len(value) == 0 {
		return errors.New("coord/kv: a trash reap report needs a value")
	}
	if _, err := f.objects.Put(ctx, trashReapKey, value); err != nil {
		return unavailable("record the trash reap", err)
	}
	return nil
}

// TrashReap reads the trash reaper's last report from the leader
// ([getLatest]): a replica behind the last pass would hand the next holder a
// schedule already spent.
func (f *FleetStore) TrashReap(ctx context.Context) ([]byte, bool, error) {
	entry, err := f.get(ctx, f.objects, trashReapKey)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, unavailable("read the trash reap", err)
	}
	return entry.Value(), true, nil
}
