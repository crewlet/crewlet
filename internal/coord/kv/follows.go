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

// The chat thread-follows, as one record per (backend, seat, channel, thread).
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
// # Why there is no purge
//
// The bucket's own age is the retention, which is the rule for every aged slot
// here. Every re-assert — a mention, a collective address, the seat posting
// into the thread — rewrites the record, so the age is a true last-activity
// stamp rather than a creation date, and the broker expires what has gone
// quiet. A sweep would have nothing to delete.

// followsClass is the key class, so a follow cannot collide with anything else
// that might one day share this bucket.
const followsClass = "follow"

// followRecord is one follow on the wire.
//
// The instant is diagnostic: an operator asking why a seat answered a thread
// reads the reason, and when it started following out of `at`. Nothing
// branches on it — the retention is the bucket's age, not this field — but a
// record that carried only a reason would make "since when" unanswerable
// without reading the broker's own metadata.
type followRecord struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// followKey composes one follow's key.
func followKey(backend, handle, channel, thread string) string {
	return coord.DocumentKey(followsClass, backend, handle, channel, thread)
}

// Follow records that a seat follows a thread, or refreshes an existing
// follow's reason and activity stamp.
//
// PUT rather than create-or-update-if-changed, and that is the retention
// working as designed: rewriting the record is what moves the bucket's age
// forward, so a thread somebody is still active in never expires while a
// thread nobody has touched in ninety days does.
func (f *FleetStore) Follow(ctx context.Context, backend, handle, channel, thread, reason string, at time.Time) error {
	if backend == "" || handle == "" || thread == "" {
		return errors.New("coord/kv: a follow needs a backend, a handle and a thread")
	}
	raw, err := json.Marshal(followRecord{Reason: reason, At: at.UTC()})
	if err != nil {
		return fmt.Errorf("coord/kv: encode the follow: %w", err)
	}
	if _, err := f.follows.Put(ctx, followKey(backend, handle, channel, thread), raw); err != nil {
		return unavailable(fmt.Sprintf("follow %s thread %s for %s", backend, thread, handle), err)
	}
	return nil
}

// FollowIfAbsent records a follow only where none exists, reporting whether
// this call created it.
//
// CREATE, NOT PUT, and the difference is the whole method: [FleetStore.Follow]
// rewrites unconditionally because rewriting is how a re-assert moves the
// bucket's age forward, and that is exactly wrong for a caller carrying rows
// that are OLDER than whatever the fleet may already hold. See
// [coord.Follows] for what depends on it.
//
// `false` with no error means the fleet already has this follow, which is a
// success for every caller this exists for.
func (f *FleetStore) FollowIfAbsent(ctx context.Context, backend, handle, channel, thread, reason string, at time.Time) (bool, error) {
	if backend == "" || handle == "" || thread == "" {
		return false, errors.New("coord/kv: a follow needs a backend, a handle and a thread")
	}
	raw, err := json.Marshal(followRecord{Reason: reason, At: at.UTC()})
	if err != nil {
		return false, fmt.Errorf("coord/kv: encode the follow: %w", err)
	}
	_, err = f.follows.Create(ctx, followKey(backend, handle, channel, thread), raw)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyExists):
		return false, nil
	default:
		return false, unavailable(
			fmt.Sprintf("record the follow on %s thread %s for %s", backend, thread, handle), err)
	}
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
	entry, err := f.follows.Get(ctx, followKey(backend, handle, channel, thread))
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

// Unfollow drops a follow, reporting whether one was there.
//
// # Why the delete is bound to the revision the read returned
//
// The report has to come from a READ, because a KV delete is idempotent and
// says nothing about what was there. That leaves a window between the two, and
// an UNCONDITIONAL delete in it reports on what the read saw while acting on
// whatever is there now. The two come apart where it matters: a peer that
// removed the key first leaves the read holding a record and the delete
// no-oping, and the caller is told `true` about a removal somebody else made.
//
// [internal/coord/memory.Fleet.Unfollow] holds one mutex across its lookup and
// its delete, so that answer is unreachable there — it reports `true` exactly
// when it removed something. One suite certifies both backends, and a twin
// that agrees only with itself proves nothing, so this one has to reach the
// same answer without a mutex it cannot have: each pass reads a revision,
// removes exactly that revision, and a pass that loses re-reads.
//
// # Why it retries rather than giving up
//
// Giving up on a mismatch is the tempting shape and it is wrong, measured
// rather than argued: against a live re-assert it answers `(false, nil)` while
// leaving the follow in place — a seat told to stop watching a thread that
// did not stop — in roughly half of a raced sample, where the twin does it in
// none. That is not a smaller divergence than the one being fixed, it is the
// same defect pointing the other way, and it contradicts the first sentence of
// this contract.
//
// The loop converges on the current record instead, which is what the twin's
// mutex buys for free and is SERIALIZABLE: every outcome it produces is one
// some sequential order of the concurrent calls would have produced. A follow
// re-asserted inside the window is therefore removed, exactly as it is when the
// re-assert loses the twin's mutex by a nanosecond. The next mention re-follows
// through the ordinary path, which is what makes that the cheap direction.
//
// DELETE RATHER THAN PURGE, which is the opposite of what a bucket with no TTL
// takes (see [FleetStore.DeleteIntegrationStatus] for that reasoning): a delete
// leaves a tombstone revision, and tombstones only accumulate for ever where
// nothing ages them out. This bucket HAS an age — `FollowRetention` — so the
// broker expires the tombstone with everything else, and a purge would roll up
// the subject for no gain.
func (f *FleetStore) Unfollow(ctx context.Context, backend, handle, channel, thread string) (bool, error) {
	if backend == "" || handle == "" || thread == "" {
		return false, nil
	}
	key := followKey(backend, handle, channel, thread)
	for range fleetCASRetries {
		entry, err := f.follows.Get(ctx, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			return false, nil
		case err != nil:
			return false, unavailable(
				fmt.Sprintf("read the follow on %s thread %s for %s", backend, thread, handle), err)
		}
		removed, err := f.deleteFollowAt(ctx, key, entry.Revision())
		if err != nil {
			return false, unavailable(
				fmt.Sprintf("unfollow %s thread %s for %s", backend, thread, handle), err)
		}
		if removed {
			return true, nil
		}
	}
	// Sixteen rounds of losing means this key is being rewritten as fast as it
	// is being read, which is not a state this engine produces: a follow is
	// written by an inbound message and unfollowed by a person. Reported
	// rather than answered, for [FleetStore.Allow]'s reason — an invented
	// answer would tell somebody they had stopped watching a thread they are
	// still watching.
	return false, contended("unfollow",
		fmt.Sprintf("%s thread %s for %s", backend, thread, handle))
}

// deleteFollowAt removes a follow only if it is still at the revision the
// caller read, reporting whether it did.
//
// THE RULE [FleetStore.Unfollow] IS BUILT FROM, in a unit that can be driven
// directly: the interleaving it exists for opens INSIDE that method, between
// its read and its write, and cannot be staged from outside it. A test that
// reached past this to the KV client would be asserting what JetStream does
// rather than what this package does.
//
// `false` with no error is a LOST RACE, not a failure — the key moved or went
// away under the read — and both are ordinary states this engine produces.
//
// TODAY'S CLIENT REPORTS BOTH SHAPES AS A MISMATCH: jetstream's Delete maps
// every expected-last-subject-sequence refusal through one path and has no
// key-not-found arm at all, so a key a peer already removed arrives here as a
// mismatch against an empty subject. The second arm is kept anyway, matching
// [FleetStore.DeleteSandboxRun] — the mapping is the client's to change and
// not ours to depend on.
func (f *FleetStore) deleteFollowAt(ctx context.Context, key string, revision uint64) (bool, error) {
	err := f.follows.Delete(ctx, key, jetstream.LastRevision(revision))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch),
		errors.Is(err, jetstream.ErrKeyNotFound):
		return false, nil
	default:
		return false, err
	}
}
