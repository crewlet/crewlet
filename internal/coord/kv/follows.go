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

// The chat thread-follows, as one record per (backend, seat, channel, thread),
// the seat named by its handle — see [coord.Follows].
//
// # Why the key is four segments and not a composed string
//
// A key IS a subject token path under its bucket, and every one of these four
// carries bytes a token cannot: a Slack channel id is safe, but a Mattermost
// root-post id, a seat handle and above all a backend-supplied thread id are
// not things this package gets to promise about. [coord.DocumentKey] escapes
// each segment and joins them, which is what keeps `a.b` distinct from a
// single segment that happened to spell it — see internal/coord/keys.go for
// what a raw segment does to a filtered watch.
//
// # Why there is no purge, and no delete
//
// The bucket's own age is the retention, which is the rule for every aged slot
// here. Every re-assert — a mention, a collective address, the seat posting
// into the thread — rewrites the record, so the age is a true last-activity
// stamp rather than a creation date, and the broker expires what has gone
// quiet. A sweep would have nothing to delete, and nothing else removes a
// follow ([coord.Follows]), so this bucket never holds a removal marker.

// followsClass is the key class, so a follow cannot collide with anything else
// that might one day share this bucket.
const followsClass = "follow"

// followRecord is one follow on the wire.
//
// The instant is diagnostic: an operator asking why a seat answered a thread
// reads the reason, and when the follow was last asserted out of `at` — every
// assert rewrites the record, so it is the follow's latest activity rather
// than its start. Nothing branches on it — the retention is the bucket's age,
// not this field — but a record that carried only a reason would make "how
// recently" unanswerable without reading the broker's own metadata.
type followRecord struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// followKey composes one follow's key.
func followKey(backend, handle, channel, thread string) string {
	return coord.DocumentKey(followsClass, backend, handle, channel, thread)
}

// Follow records that a seat follows a thread, or refreshes the follow it has,
// writing the reason choose answers for the record it read — see
// [coord.Follows] for the contract.
//
// A WRITE BOUND TO THE REVISION IT READ: the reason is decided from a read,
// and an unconditional Put after it lands over whatever another node wrote
// between the two. That was this method's shape, and the record went to
// whichever writer was LAST rather than to the reason the caller would have
// kept — a mention a peer recorded in the window was overwritten by a
// broadcast decided before it existed. Each pass now reads the leader, asks
// choose, and writes at exactly that revision; a pass that loses re-reads, so
// the record converges on a choice made against what it holds.
//
// STILL A WRITE ON EVERY CALL, and that is the retention working as designed:
// rewriting the record is what moves the bucket's age forward, so a follow the
// seat is still asserted into — named, shouted at or posting there — never
// expires, while one nothing has re-asserted in ninety days does. A reply that
// merely rides the follow is not an assert and moves nothing.
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
		// Every attempt lost means this key is being rewritten as fast as
		// it is being read, which is not a state this engine produces: a
		// follow is asserted by a message that names the seat in the
		// thread, a collective address, or the seat's own post there.
		// Reported rather than answered: an invented answer would tell the
		// caller a reason was recorded that no record holds.
		return "", contended("follow", what)
	}
	return wrote, nil
}

// readFollow reads one follow as the stream leader holds it, answering its
// reason, whether a follow is held, and the revision a write over it must
// name — zero for a key holding no live value.
//
// A RECORD THAT DOES NOT DECODE is answered as no follow at ITS revision, so
// the write that follows replaces exactly it. [FleetStore.Following] raises
// on one, which is right for a read; here the caller is about to write the
// key whole, and refusing would leave a seat unable ever to follow a thread
// whose record was damaged.
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
// caller read — or, at revision zero, still holds no live value — reporting
// whether it did.
//
// THE RULE [FleetStore.Follow] IS BUILT FROM, in a unit that can be driven
// directly: the interleaving it exists for opens INSIDE that method, between
// its read and its write, and cannot be staged from outside it. A test that
// reached past this to the KV client would be asserting what JetStream does
// rather than what this package does.
//
// `false` with no error is a LOST RACE — another writer moved the key, or
// created it, under the read — and never a failure.
func (f *FleetStore) writeFollowAt(ctx context.Context, key string, revision uint64, raw []byte) (bool, error) {
	var err error
	if revision == 0 {
		// Through the package's one create, which conditions on what the
		// LEADER holds: a key the bucket's age removed is written over
		// rather than reported held.
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
// THREE ANSWERS. A missing key is definitively not following; an unreadable
// store is UNKNOWN and is raised rather than flattened into false, because the
// caller's fail-closed choice is only safe while it is the caller making it.
// See [coord.Follows].
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
