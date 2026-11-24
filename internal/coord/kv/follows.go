package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// The chat thread-follows, one record per (backend, seat, channel, thread),
// the seat named by its handle; see [coord.Follows].
//
// The key is four segments, not a composed string: a key is a subject token
// path, and a Mattermost root-post id, a seat handle or a backend's thread id
// may carry bytes a token cannot. [coord.DocumentKey] escapes and joins each
// segment, so `a.b` stays distinct from one segment spelling it (coord/keys.go).
//
// There is no purge and no delete: the bucket's age is the retention. Every
// re-assert rewrites the record, so the age is last activity and the broker
// expires what has gone quiet; nothing else removes a follow, so the bucket
// never holds a removal marker.

// followsClass is the key class, so a follow cannot collide with anything else
// that might one day share this bucket.
const followsClass = "follow"

// followRecord is one follow on the wire.
//
// At is diagnostic: every assert rewrites the record, so it is the follow's
// latest activity, and answers "how recently" without the broker's metadata.
// Nothing branches on it; the retention is the bucket's age.
type followRecord struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// followKey composes one follow's key.
func followKey(backend, handle, channel, thread string) string {
	return coord.DocumentKey(followsClass, backend, handle, channel, thread)
}

// Follow records that a seat follows a thread, or refreshes the follow it has,
// writing the reason choose answers for the record it read; see
// [coord.Follows] for the contract.
//
// The write is BOUND TO THE REVISION IT READ: an unconditional Put would land
// over a peer's write between the two (a broadcast decided before a mention,
// over the mention). A pass that loses re-reads, so the record converges on a
// choice made against what it holds.
//
// It writes on every call by design: the rewrite moves the bucket's age, so a
// follow still asserted into never expires, while a quiet one does. A reply
// that merely rides the follow is not an assert and moves nothing.
func (f *FleetStore) Follow(ctx context.Context, backend, handle, channel, thread string, at time.Time,
	choose func(held string, following bool) string) (string, error) {

	if backend == "" || handle == "" || thread == "" {
		return "", errors.New("coord/kv: a follow needs a backend, a handle and a thread")
	}
	key := followKey(backend, handle, channel, thread)
	what := fmt.Sprintf("%s thread %s for %s", backend, thread, handle)
	var wrote string
	settled, err := casRetry(ctx, &f.followTrips, "follow "+what, func() (bool, error) {
		held, following, revision, err := f.readFollow(ctx, key)
		if err != nil {
			return false, unavailable("read the follow on "+what, err)
		}
		reason := choose(held, following)
		if err = coord.CheckFollowReason(reason); err != nil {
			return false, err
		}
		raw, err := json.Marshal(followRecord{Reason: reason, At: at.UTC()})
		if err != nil {
			return false, fmt.Errorf("coord/kv: encode the follow: %w", err)
		}
		landed, err := f.writeFollowAt(ctx, key, revision, raw)
		if err != nil {
			return false, unavailable("follow "+what, err)
		}
		if !landed {
			return false, nil
		}
		wrote = reason
		return true, nil
	})
	switch {
	case err != nil:
		return "", err
	case !settled:
		// Every attempt lost means the key is rewritten as fast as it is
		// read, which this engine does not produce. Reported rather than
		// answered: an invented answer would claim a reason no record holds.
		return "", contended("follow", what)
	}
	return wrote, nil
}

// readFollow reads one follow as the stream leader holds it: its reason,
// whether one is held, and the revision a write over it must name (zero for
// no live value).
//
// A RECORD THAT DOES NOT DECODE is no follow at ITS revision, so the write
// replaces exactly it. [FleetStore.Following] raises on one, as a read should;
// refusing here would leave a seat unable ever to follow that thread.
func (f *FleetStore) readFollow(ctx context.Context, key string) (string, bool, uint64, error) {
	entry, err := f.get(ctx, f.follows, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return "", false, 0, nil
	case err != nil:
		return "", false, 0, err
	}
	var rec followRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		// Loudly, as every listing skips one: the write that follows
		// replaces it, and nothing else would ever say it was damaged.
		log.WarnContext(ctx, "coord_kv_undecodable_record", "bucket", f.follows.Bucket(),
			"key", key, "error", err.Error())
		//nolint:nilerr // A damaged record is one the caller's write replaces, not a failed read.
		return "", false, entry.Revision(), nil
	}
	return rec.Reason, true, entry.Revision(), nil
}

// writeFollowAt writes a follow only if the key is still at the revision the
// caller read (at zero, still holds no live value), reporting whether it did.
// It is [FleetStore.Follow]'s rule as a unit a test can drive: the race opens
// between Follow's read and write, and cannot be staged from outside.
//
// `false` with no error is a LOST RACE, never a failure.
func (f *FleetStore) writeFollowAt(ctx context.Context, key string, revision uint64, raw []byte) (bool, error) {
	var err error
	if revision == 0 {
		// The package's one create conditions on what the LEADER holds, so a
		// key the bucket's age removed is written over, not reported held.
		_, err = f.create(ctx, f.follows, key, raw)
		if errors.Is(err, jetstream.ErrKeyExists) {
			return false, nil
		}
	} else {
		_, err = f.follows.Update(ctx, key, raw, revision)
		if lostUpdateRace(err) {
			return false, nil
		}
	}
	return err == nil, err
}

// Following reports why a seat follows a thread, and whether it does.
//
// THREE ANSWERS: a missing key is definitively not following; an unreadable
// store is UNKNOWN and raised, not flattened into false, since the fail-closed
// choice is only safe while the caller makes it. See [coord.Follows].
func (f *FleetStore) Following(ctx context.Context, backend, handle, channel, thread string) (string, bool, error) {
	if backend == "" || handle == "" || thread == "" {
		return "", false, nil
	}
	entry, err := f.get(ctx, f.follows, followKey(backend, handle, channel, thread))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, unavailable(
			fmt.Sprintf("read the follow on %s thread %s for %s", backend, thread, handle), err)
	}
	var rec followRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return "", false, fmt.Errorf("coord/kv: decode the follow on %s thread %s for %s: %w",
			backend, thread, handle, err)
	}
	return rec.Reason, true, nil
}
