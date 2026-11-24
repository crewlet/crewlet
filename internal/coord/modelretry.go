package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// A model retry is a person's request that every node stop sitting a model
// entry out ("retry now" or "clear" on the Models page). The whole company
// has to agree on it: a sit-out is each node's own memory of a vendor's limit
// (internal/providers/llm/harness's Plan), so it is one record per entry key
// in the coordination store, watched by every node (internal/engine/modelretry.go).
//
// A STAMP, NOT A COMMAND: a node clears the windows opened by reports dated
// BEFORE it, and a later report stands. That is order-free and idempotent, so
// the record needs no consumer bookkeeping and no age (it lives in the
// positions register); the apply that removes the entry deletes it. A write
// keeps the LATER stamp ([ModelRetries.PutModelRetry]), whose clear is a
// superset, so nobody's retry is taken back.

// ModelRetryMode is how a retry was asked for.
type ModelRetryMode string

const (
	// RetryProbe is "retry now": the node that took the request made one
	// call on the entry first, and wrote the record only because it
	// answered.
	RetryProbe ModelRetryMode = "probe"
	// RetryClear is "clear": no call was made; the person said the limit
	// is over.
	RetryClear ModelRetryMode = "clear"
)

// Valid reports whether the mode is one this build names.
func (m ModelRetryMode) Valid() bool { return m == RetryProbe || m == RetryClear }

// ModelRetry is one entry's latest retry request, as the coordination store
// holds it.
type ModelRetry struct {
	// Key is the entry's key in `providers.llm`, and the record's key.
	Key string `json:"key"`

	// Mode is how it was asked for.
	Mode ModelRetryMode `json:"mode"`

	// At is when it was asked for: every node clears the windows opened by
	// reports dated before it.
	At time.Time `json:"at"`

	// By, ByKind and OperatorID are who asked, as [SeatPause] records them.
	By         string        `json:"by"`
	ByKind     iam.ActorKind `json:"by_kind"`
	OperatorID string        `json:"operator_id,omitempty"`

	// Version is the store's version of the record as it was read. Opaque,
	// as [SeatPause.Version] is. Not on the wire.
	Version uint64 `json:"-"`
}

// Validate reports why a retry cannot be written.
func (r ModelRetry) Validate() error {
	switch {
	case r.Key == "":
		return errors.New("coord: a model retry needs the key of the entry it retries")
	case !r.Mode.Valid():
		return errors.New("coord: a model retry needs its mode: probe or clear")
	case r.At.IsZero():
		return errors.New("coord: a model retry needs the instant it was asked for: " +
			"every node clears what was reported before it")
	case r.By == "":
		return errors.New("coord: a model retry needs who asked for it")
	case !r.ByKind.Valid():
		return errors.New("coord: a model retry needs what sort of author asked for it " +
			"(iam.ActorFor's kind): agent, human, operator or system")
	case len(r.Key) > MaxModelEntryBytes:
		return fmt.Errorf("coord: a model retry names an entry past %d bytes", MaxModelEntryBytes)
	}
	return checkActor("a model retry", r.By, r.OperatorID)
}

// MaxModelEntryBytes bounds the key of a model entry a record names
// (providers.llm's entries, by provider and name).
const MaxModelEntryBytes = 256

// ModelRetryClass is the key class a retry is filed under in the positions
// register.
const ModelRetryClass = "llm_retry"

// ModelRetryKey is an entry's retry key in the register.
func ModelRetryKey(entry string) string { return DocumentKey(ModelRetryClass, entry) }

// ModelRetryEntry recovers the entry's key from a key of the retry class,
// reporting false for a key of another class or one this grammar did not
// write.
func ModelRetryEntry(key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != ModelRetryClass || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}

// ModelRetryUpdate is one change a retry watch delivers.
type ModelRetryUpdate struct {
	// Retry is the record the entry now holds, nil when it was deleted.
	Retry *ModelRetry

	// Key names the entry the update is about. Empty on the marker.
	Key string

	// Current marks the end of the records that existed when the watch
	// started, as [SeatPauseUpdate.Current] does.
	Current bool
}

// ModelRetries is the fleet's record of the retries people asked for.
//
// Every read RAISES rather than answering empty when the store cannot be
// read: a missed retry only lets a sit-out run to its end, but raising keeps
// a reader from confusing that with no retry at all.
type ModelRetries interface {
	// ModelRetry reads one entry's retry.
	ModelRetry(ctx context.Context, entry string) (ModelRetry, bool, error)

	// ListModelRetries returns every retry, ordered by entry key.
	ListModelRetries(ctx context.Context) ([]ModelRetry, error)

	// PutModelRetry records a retry, keeping whichever of it and the
	// stored one is LATER, and returns the record the entry holds after
	// the call. A write that loses to a later stamp changes nothing.
	PutModelRetry(ctx context.Context, r ModelRetry) (ModelRetry, error)

	// DeleteModelRetry removes an entry's retry at a version, reporting
	// whether that version still held. A version of zero never holds.
	DeleteModelRetry(ctx context.Context, entry string, version uint64) (bool, error)

	// WatchModelRetries streams every retry, then the
	// [ModelRetryUpdate.Current] marker, then every change after it. A
	// closed channel is a watcher that stopped hearing, as
	// [SeatPauses.WatchSeatPauses] says.
	WatchModelRetries(ctx context.Context) (<-chan ModelRetryUpdate, error)
}
