package coord

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// A DEVICE SIGN-IN is a CLI's device-code login in progress for one model
// entry: the engine runs `codex login --device-auth`, relays the verification
// address and the one-time code, and a person approves it in their browser.
//
// # Who has to agree on it
//
// The whole company, now. One node runs the CLI's process, but any node may
// serve the status read the dashboard polls, a cancel may arrive at any node,
// and a second start for the same entry must be refused fleet-wide: two
// processes signing one entry in would race to write one login. So the
// session is one record in the coordination store, written by the node running
// it and watched by every node (internal/engine/devicesignin.go).
//
// # One record per entry
//
// Filed under the ENTRY's key, not the session's id, so "one session per entry
// across the fleet" is the store's own create-if-absent rather than a second
// lock record kept in step with the first. A start over a record whose session
// has ENDED (or lapsed, or whose node is gone) replaces it at the version read.
// The status read by id lists the class, which holds one record per entry the
// company signs in this way.
//
// # An expiry the readers keep, and no bucket age
//
// A session outlives its process only long enough for the page that started it
// to read how it ended: until [DeviceSignIn.ExpiresAt] plus
// [DeviceSignInLinger]. That is a per-record lifetime, which a bucket age
// cannot hold (an update would restart it; internal/coord/kv's package doc has
// the measurement), so the record lives in the positions register, the bucket
// with no age, and is LAPSED by its own fields: [DeviceSignIn.Lapsed] is "as
// good as gone", every reader answers it absent, and every node's watch
// removes the lapsed records it sees. A node that dies mid-session leaves a
// record no process will end; every other node marks it failed once the dead
// node's presence lease is gone, so nobody waits out a code nobody is reading.

// DeviceSignInStatus is where a device sign-in is.
type DeviceSignInStatus string

const (
	// DeviceStarting is a session whose process has not printed its code.
	DeviceStarting DeviceSignInStatus = "starting"
	// DevicePending is a session waiting for the person to approve the
	// code.
	DevicePending DeviceSignInStatus = "pending"
	// DeviceCancelRequested is a pending session somebody cancelled
	// through a node that does not run it; the running node ends it.
	DeviceCancelRequested DeviceSignInStatus = "cancel_requested"
	// DeviceApproved is a session whose login was stored and activated.
	DeviceApproved DeviceSignInStatus = "approved"
	// DeviceFailed is a session that ended without a login.
	DeviceFailed DeviceSignInStatus = "failed"
	// DeviceExpired is a session whose code ran out unapproved.
	DeviceExpired DeviceSignInStatus = "expired"
	// DeviceCancelled is a session that was cancelled.
	DeviceCancelled DeviceSignInStatus = "cancelled"
)

// DeviceSignInStatuses is every status this build names, in a session's order:
// the closed set [DeviceSignInStatus.Valid] reads, and the one a client's
// copy of the words is held to.
var DeviceSignInStatuses = []DeviceSignInStatus{
	DeviceStarting, DevicePending, DeviceCancelRequested,
	DeviceApproved, DeviceFailed, DeviceExpired, DeviceCancelled,
}

// Valid reports whether the status is one this build names.
func (s DeviceSignInStatus) Valid() bool { return slices.Contains(DeviceSignInStatuses, s) }

// Ended reports whether the session is over: its process is gone and nothing
// will change the record again.
func (s DeviceSignInStatus) Ended() bool {
	switch s {
	case DeviceApproved, DeviceFailed, DeviceExpired, DeviceCancelled:
		return true
	}
	return false
}

// DeviceSignInLinger is how long a session's record answers after its code
// expires: ten minutes, so a page that polls every few seconds, or a person
// who looked away, still reads how it ended.
const DeviceSignInLinger = 10 * time.Minute

// DeviceSignIn is one entry's device sign-in, as the coordination store holds
// it.
type DeviceSignIn struct {
	// ID is the session's id, what the status read and the cancel name.
	ID uuid.UUID `json:"id"`

	// Entry is the model entry's key, and the record's key.
	Entry string `json:"entry"`

	// Node is the node running the CLI's process.
	Node string `json:"node"`

	// URL is the verification address and Code the one-time code the
	// person types there, empty until the process prints them.
	URL  string `json:"url,omitempty"`
	Code string `json:"code,omitempty"`

	// Status is where the session is, and Detail why it failed (the CLI's
	// own line, where it said one).
	Status DeviceSignInStatus `json:"status"`
	Detail string             `json:"detail,omitempty"`

	// StartedAt is when it started and ExpiresAt when its code runs out.
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`

	// RevisionID and Epoch are the activation the approved login's
	// re-activation produced, empty when it produced none.
	RevisionID string `json:"revision_id,omitempty"`
	Epoch      uint64 `json:"epoch,omitempty"`

	// By, ByKind and OperatorID are who started it, as [SeatPause]
	// records them.
	By         string        `json:"by"`
	ByKind     iam.ActorKind `json:"by_kind"`
	OperatorID string        `json:"operator_id,omitempty"`

	// Version is the store's version of the record as it was read. Not on
	// the wire.
	Version uint64 `json:"-"`
}

// Validate reports why a session cannot be written.
func (s DeviceSignIn) Validate() error {
	switch {
	case s.ID == uuid.Nil:
		return errors.New("coord: a device sign-in needs its session id")
	case s.Entry == "":
		return errors.New("coord: a device sign-in needs the key of the entry it signs in")
	case s.Node == "":
		return errors.New("coord: a device sign-in needs the node running it: a cancel " +
			"and a takeover are both addressed to that node")
	case !s.Status.Valid():
		return errors.New("coord: a device sign-in needs a status this build names")
	case s.StartedAt.IsZero() || s.ExpiresAt.IsZero():
		return errors.New("coord: a device sign-in needs when it started and when its code expires")
	case s.By == "" || !s.ByKind.Valid():
		return errors.New("coord: a device sign-in needs who started it")
	}
	return nil
}

// Lapsed reports whether the record is past its lifetime at now: its code
// expired more than [DeviceSignInLinger] ago. A lapsed record is read as
// absent everywhere.
func (s DeviceSignIn) Lapsed(now time.Time) bool {
	return now.After(s.ExpiresAt.Add(DeviceSignInLinger))
}

// DeviceSignInClass is the key class a session is filed under in the positions
// register.
const DeviceSignInClass = "llm_device_sign_in"

// DeviceSignInKey is an entry's session key in the register.
func DeviceSignInKey(entry string) string { return DocumentKey(DeviceSignInClass, entry) }

// DeviceSignInEntry recovers the entry's key from a key of the session class.
func DeviceSignInEntry(key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != DeviceSignInClass || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}

// DeviceSignInUpdate is one change a session watch delivers.
type DeviceSignInUpdate struct {
	// SignIn is the record the entry now holds, nil when it was removed.
	SignIn *DeviceSignIn

	// Entry names the entry the update is about. Empty on the marker.
	Entry string

	// Current marks the end of the records that existed when the watch
	// started, as [SeatPauseUpdate.Current] does.
	Current bool
}

// DeviceSignIns is the fleet's record of the device sign-ins in progress.
//
// The store answers what it holds; whether a record has [DeviceSignIn.Lapsed]
// is the reader's to judge against its own clock, since the store has none.
type DeviceSignIns interface {
	// DeviceSignIn reads one entry's session.
	DeviceSignIn(ctx context.Context, entry string) (DeviceSignIn, bool, error)

	// ListDeviceSignIns returns every session, ordered by entry key.
	ListDeviceSignIns(ctx context.Context) ([]DeviceSignIn, error)

	// CreateDeviceSignIn writes a session for an entry that has no record,
	// returning it as stored. An entry that has one is left alone and
	// reports false: only an update at the version its caller read may
	// replace it.
	CreateDeviceSignIn(ctx context.Context, s DeviceSignIn) (DeviceSignIn, bool, error)

	// UpdateDeviceSignIn writes s at s.Version, reporting false when that
	// version no longer holds, including when the record is gone.
	UpdateDeviceSignIn(ctx context.Context, s DeviceSignIn) (DeviceSignIn, bool, error)

	// DeleteDeviceSignIn removes an entry's session at a version,
	// reporting whether that version still held. A version of zero never
	// holds.
	DeleteDeviceSignIn(ctx context.Context, entry string, version uint64) (bool, error)

	// WatchDeviceSignIns streams every session, then the
	// [DeviceSignInUpdate.Current] marker, then every change after it.
	WatchDeviceSignIns(ctx context.Context) (<-chan DeviceSignInUpdate, error)
}
