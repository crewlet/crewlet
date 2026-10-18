package kv

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"
)

// ---- the trash reaper ------------------------------------------------- //

// trashReapKey is the trash reaper's last report, kept in the objects bucket
// beside the object collector's ([collectionKey]).
//
// THAT BUCKET, rather than one of its own, because the two are the same kind
// of record: a reclamation duty's whole account of its last pass, replaced by
// each pass and read by whichever node takes the duty next. Both need the
// bucket's one property, NO AGE: a report that expired would read as a duty
// that never ran, and the reaper would take that as a pass due at once rather
// than at the time the last holder recorded. A bucket of its own would buy
// nothing but one more stream every node provisions at boot.
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

// TrashReap reads the trash reaper's last report from the leader, for the
// reason every single-key read here does ([getLatest]): a replica behind the
// last pass would hand a node taking the duty a schedule already spent.
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
