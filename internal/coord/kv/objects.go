package kv

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"
)

// ---- the object store ------------------------------------------------- //

// objectBackendKey and collectionKey are the objects bucket's two keys: the
// fleet's recorded object backend, and the collector's last report.
const (
	objectBackendKey = "backend"
	collectionKey    = "collection"
)

// AgreeObjectBackend records identity unless a backend is recorded, and
// answers the recorded one ([coord.ObjectStores]).
//
// A refused create is read back FROM THE LEADER, for the custody claim's
// reason: a replica behind the create would answer that nothing is recorded,
// and the caller would boot against a backend it never compared. A record
// gone between the refusal and the read is created again, for the fleet
// store's patience ([FleetStore.retryRaces]).
func (f *FleetStore) AgreeObjectBackend(ctx context.Context, identity string) (string, error) {
	if identity == "" {
		return "", errors.New("coord/kv: an object backend needs an identity")
	}
	var agreed string
	err := f.retryRaces(ctx, "AgreeObjectBackend", objectBackendKey, func() (bool, error) {
		_, err := f.create(ctx, f.objects, objectBackendKey, []byte(identity))
		switch {
		case err == nil:
			agreed = identity
			return false, nil
		case !errors.Is(err, jetstream.ErrKeyExists):
			return false, unavailable("record the object backend", err)
		}
		entry, err := f.get(ctx, f.objects, objectBackendKey)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			return true, nil
		case err != nil:
			return false, unavailable("read the object backend", err)
		}
		if len(entry.Value()) == 0 {
			return false, errors.New("coord/kv: the recorded object backend is empty")
		}
		agreed = string(entry.Value())
		return false, nil
	})
	if err != nil {
		return "", err
	}
	return agreed, nil
}

// RecordObjectCollection stores the collector's last report, replacing the
// one before: one writer at a time holds the duty, and a report is a whole
// account rather than a delta, so the last write is the answer.
func (f *FleetStore) RecordObjectCollection(ctx context.Context, value []byte) error {
	if len(value) == 0 {
		return errors.New("coord/kv: a collection report needs a value")
	}
	if _, err := f.objects.Put(ctx, collectionKey, value); err != nil {
		return unavailable("record the object collection", err)
	}
	return nil
}

// ObjectCollection reads the collector's last report from the leader.
func (f *FleetStore) ObjectCollection(ctx context.Context) ([]byte, bool, error) {
	entry, err := f.get(ctx, f.objects, collectionKey)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, unavailable("read the object collection", err)
	}
	return entry.Value(), true, nil
}
