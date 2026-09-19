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
// The report is taken from a READ BEFORE the delete rather than from the
// delete itself, because a KV delete is idempotent and says nothing about what
// was there. The race it leaves — two unfollows at once both reporting true —
// is one nobody acts on: the caller uses the answer to tell a person "you were
// not watching that" and both answers are true of the state they observed.
func (f *FleetStore) Unfollow(ctx context.Context, backend, handle, channel, thread string) (bool, error) {
	if backend == "" || handle == "" || thread == "" {
		return false, nil
	}
	key := followKey(backend, handle, channel, thread)
	_, err := f.follows.Get(ctx, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return false, nil
	case err != nil:
		return false, unavailable(
			fmt.Sprintf("read the follow on %s thread %s for %s", backend, thread, handle), err)
	}
	if err := f.follows.Delete(ctx, key); err != nil {
		return false, unavailable(
			fmt.Sprintf("unfollow %s thread %s for %s", backend, thread, handle), err)
	}
	return true, nil
}
