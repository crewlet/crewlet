package kv

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"

	"github.com/nats-io/nats.go/jetstream"
)

// ---- the object store ------------------------------------------------- //

// objectBackendKey and collectionKey are the objects bucket's keys for the
// object store: the fleet's recorded object backend, and the collector's last
// report. The trash reaper's report shares the bucket ([trashReapKey]).
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
// gone between the refusal and the read — which only an operator removing it
// does, since the bucket has no age — is created again, within the shared
// bound ([casRetry], [fleetCASAttempts]).
func (f *FleetStore) AgreeObjectBackend(ctx context.Context, identity string) (string, error) {
	if identity == "" {
		return "", errors.New("coord/kv: an object backend needs an identity")
	}
	var agreed string
	settled, err := casRetry(ctx, &f.objectTrips, "record the object backend", func() (bool, error) {
		_, err := f.create(ctx, f.objects, objectBackendKey, []byte(identity))
		switch {
		case err == nil:
			agreed = identity
			return true, nil
		case !errors.Is(err, jetstream.ErrKeyExists):
			return false, unavailable("record the object backend", err)
		}
		entry, err := f.get(ctx, f.objects, objectBackendKey)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// GONE between the refusal and the read, which only an
			// operator removing the record does: the bucket has no age.
			return false, nil
		case err != nil:
			return false, unavailable("read the object backend", err)
		}
		if len(entry.Value()) == 0 {
			return false, errors.New("coord/kv: the recorded object backend is empty")
		}
		agreed = string(entry.Value())
		return true, nil
	})
	switch {
	case err != nil:
		return "", err
	case !settled:
		return "", contended("AgreeObjectBackend", objectBackendKey)
	}
	return agreed, nil
}

// MoveObjectBackend replaces the recorded backend from with to, by a
// compare-and-set on the revision it read from the leader
// ([coord.ObjectStores]).
//
// Its contenders are an operator's own moves and nothing else, since the
// record is otherwise written once, by the first node to boot against a store
// ([FleetStore.AgreeObjectBackend]) — so it is far inside the shared bound
// ([casRetry], [fleetCASAttempts]), and a lost compare-and-set is read again
// and judged against what won: a move another operator already made to the
// same store is the answer, and one made elsewhere is [coord.ErrObjectBackendMoved].
func (f *FleetStore) MoveObjectBackend(ctx context.Context, from, to string) error {
	if from == "" || to == "" {
		return errors.New("coord/kv: a move of the object backend names both stores")
	}
	settled, err := casRetry(ctx, &f.objectTrips, "move the object backend", func() (bool, error) {
		entry, err := f.get(ctx, f.objects, objectBackendKey)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			return false, fmt.Errorf("%w: no backend is recorded", coord.ErrObjectBackendMoved)
		case err != nil:
			return false, unavailable("read the object backend", err)
		}
		switch string(entry.Value()) {
		case to:
			return true, nil
		case from:
		default:
			return false, fmt.Errorf("%w: %q is recorded", coord.ErrObjectBackendMoved, entry.Value())
		}
		_, err = f.objects.Update(ctx, objectBackendKey, []byte(to), entry.Revision())
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			return false, nil
		default:
			return false, unavailable("move the object backend", err)
		}
	})
	switch {
	case err != nil:
		return err
	case !settled:
		return contended("MoveObjectBackend", objectBackendKey)
	}
	return nil
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
