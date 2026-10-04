package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
// and the caller would boot against a backend it never compared.
func (f *FleetStore) AgreeObjectBackend(ctx context.Context, identity string) (string, error) {
	if identity == "" {
		return "", errors.New("coord/kv: an object backend needs an identity")
	}
	for range fleetCASRetries {
		_, err := f.create(ctx, f.objects, objectBackendKey, []byte(identity))
		switch {
		case err == nil:
			return identity, nil
		case !errors.Is(err, jetstream.ErrKeyExists):
			return "", unavailable("record the object backend", err)
		}
		entry, err := f.get(ctx, f.objects, objectBackendKey)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			continue
		case err != nil:
			return "", unavailable("read the object backend", err)
		}
		if len(entry.Value()) == 0 {
			return "", errors.New("coord/kv: the recorded object backend is empty")
		}
		return string(entry.Value()), nil
	}
	return "", contended("AgreeObjectBackend", objectBackendKey)
}

// chunkLockRecord is one held chunk lock on the wire. The instant is
// diagnostic: the bucket's age is what lets a lock go.
type chunkLockRecord struct {
	Owner string    `json:"owner"`
	At    time.Time `json:"at"`
}

// LockChunk takes chunk's lock for owner ([coord.ObjectStores]).
func (f *FleetStore) LockChunk(ctx context.Context, chunk, owner string) (bool, error) {
	if chunk == "" || owner == "" {
		return false, errors.New("coord/kv: a chunk lock needs a chunk and an owner")
	}
	raw, err := json.Marshal(chunkLockRecord{Owner: owner, At: time.Now().UTC()})
	if err != nil {
		return false, fmt.Errorf("coord/kv: encode the chunk lock: %w", err)
	}
	key := encodeKey(chunk)
	for range fleetCASRetries {
		_, err = f.create(ctx, f.chunkLocks, key, raw)
		switch {
		case err == nil:
			return true, nil
		case !errors.Is(err, jetstream.ErrKeyExists):
			return false, unavailable("lock chunk "+chunk, err)
		}
		held, _, found, err := f.chunkLock(ctx, key)
		switch {
		case err != nil:
			return false, err
		case !found:
			continue
		}
		// A LOCK THIS OWNER ALREADY HOLDS is answered held, so a take
		// retried after its answer was lost is not refused by itself.
		return held.Owner == owner, nil
	}
	return false, contended("LockChunk", chunk)
}

// UnlockChunk lets go of chunk's lock if owner holds it, conditioned on the
// revision it read, so a lock that aged out and was taken by another owner in
// between is never deleted from under them.
func (f *FleetStore) UnlockChunk(ctx context.Context, chunk, owner string) error {
	key := encodeKey(chunk)
	held, revision, found, err := f.chunkLock(ctx, key)
	if err != nil || !found || held.Owner != owner {
		return err
	}
	err = f.chunkLocks.Delete(ctx, key, jetstream.LastRevision(revision))
	if err != nil && !isWrongLastSequence(err) && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("unlock chunk "+chunk, err)
	}
	return nil
}

// chunkLock reads one chunk lock from the leader, reporting false when none is
// held.
func (f *FleetStore) chunkLock(ctx context.Context, key string) (chunkLockRecord, uint64, bool, error) {
	entry, err := f.get(ctx, f.chunkLocks, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return chunkLockRecord{}, 0, false, nil
	case err != nil:
		return chunkLockRecord{}, 0, false, unavailable("read a chunk lock", err)
	}
	var held chunkLockRecord
	if err := json.Unmarshal(entry.Value(), &held); err != nil {
		return chunkLockRecord{}, 0, false, fmt.Errorf("coord/kv: a chunk lock is unreadable: %w", err)
	}
	return held, entry.Revision(), true, nil
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
