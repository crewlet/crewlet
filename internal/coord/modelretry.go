package coord

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// A MODEL RETRY is a person's request that every node stop sitting a model
// entry out: "retry now" or "clear" on the Models page.
//
// # Who has to agree on it
//
// The whole company, now. A sit-out is each node's own memory of a vendor's
// limit (internal/providers/llm/harness's Plan), so a request served by one
// node changes nothing on another unless every node hears it. It is one record
// per entry key in the coordination store, and every node keeps a watched copy
// of all of them (internal/engine/modelretry.go), as it does of seat pauses.
//
// # A stamp, not a command
//
// The record carries the instant it was asked for, and a node clears the
// windows opened by reports dated BEFORE it. A report after the request stands:
// the vendor said the limit is still there after the person asked. That makes
// the rule order-free and idempotent: a node that hears the record twice, late,
// or after a restart clears exactly what it would have cleared on time, and a
// record left standing for a week clears nothing that was reported since. So
// the record needs no consumer bookkeeping and no expiry; the apply that
// removes the entry deletes it.
//
// # No age
//
// It lives in the positions register, the bucket with no retention, for the
// reason above: it ages out of meaning on its own, and an age would add
// nothing but a second clock.
//
// # The latest request wins
//
// Two people retrying one entry at once both want the same thing, and the
// later stamp clears a superset of what the earlier one does. So a write keeps
// whichever stamp is LATER ([ModelRetries.PutModelRetry]): a request that
// lands after a later one changes nothing, and nobody's retry is taken back.

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
	}
	return nil
}

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
// Every read RAISES rather than answering empty when the store cannot be read,
// though the safe direction is milder than a pause's: a retry a node missed is
// a sit-out that runs to its own end, which is what happens with no retry at
// all. Raising keeps a reader from confusing the two.
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
