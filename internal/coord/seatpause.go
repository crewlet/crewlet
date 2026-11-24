package coord

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// A SEAT PAUSE stops one seat taking work until somebody lifts it: a person's
// decision, or the engine's for a failure a retry cannot cure.
//
// The whole company has to agree on it — the node serving the request, the one
// holding the seat and any placement moves it to — so it is one record per seat
// in the coordination store, watched by every node (internal/engine/seatpause.go),
// keyed on the seat's agent id (ADR-0013) as its inbox is. It lives in the
// positions register, which never ages a key: an expiring pause would be a resume
// nobody chose. Only a resume or the seat's removal clears it. Writes are
// compare-and-set so no resume is lost to a racing pause (a pause CREATEs, an
// amendment or resume writes at the version read), and only the winner announces,
// keeping `seat_paused` and `seat_resumed` exactly once per change.

// SeatPause is one paused seat, as the coordination store holds it.
type SeatPause struct {
	// Seat is the paused seat's id — its agent id — and the record's key.
	Seat uuid.UUID `json:"seat"`

	// By, ByKind and OperatorID are the request's [iam.Actor], as every
	// other write by the principal records it: a person bound to a seat
	// pauses AS that seat (kind human) with their credential beside it, an
	// unbound credential under its own login (kind operator). An audit asks
	// about the credential; a screen names the author.
	By         string        `json:"by"`
	ByKind     iam.ActorKind `json:"by_kind"`
	OperatorID string        `json:"operator_id,omitempty"`

	// Reason is why, in the pauser's own words. Optional.
	Reason string `json:"reason,omitempty"`

	// StopRunning also ends the seat's running work: its turn at the next
	// round boundary, its executor at once (sandbox.Coordinator.StopSeat).
	// Without it a pause stops only NEW work.
	StopRunning bool `json:"stop_running,omitempty"`

	// At is when the seat was paused.
	At time.Time `json:"at"`

	// Failure is the failure the engine paused the seat for, nil on a pause a
	// person or an agent took: an apply that changes what it depends on lifts it.
	Failure *SeatFailure `json:"failure,omitempty"`
	// Version is the store's version of the record as read: OPAQUE, like
	// [MailboxRecord.Version], passed back as handed out. Not on the wire;
	// ignored by CreateSeatPause.
	Version uint64 `json:"-"`
}

// SeatFailure is a failure that will repeat until a person changes something,
// which the engine paused a seat for rather than retry it.
type SeatFailure struct {
	// Kind names the failure in the engine's closed table of them.
	Kind string `json:"kind"`
	// Basis is a fingerprint of the settings the failure depends on.
	Basis string `json:"basis"`
}

// Validate reports why a pause cannot be written.
//
// An unknown author kind is refused on the way IN only: a record read back
// keeps whatever kind it carries ([iam.ActorKind.Valid] tells a reader).
func (p SeatPause) Validate() error {
	switch {
	case p.Seat == uuid.Nil:
		return errors.New("coord: a seat pause needs the id of the seat it pauses")
	case p.By == "":
		return errors.New("coord: a seat pause needs who paused it: a pause nobody " +
			"can be named for is one nobody can be asked about")
	case !p.ByKind.Valid():
		return errors.New("coord: a seat pause needs what sort of author paused it " +
			"(iam.ActorFor's kind): agent, human, operator or system")
	case p.At.IsZero():
		return errors.New("coord: a seat pause needs the instant it was taken")
	case p.Failure != nil && (p.Failure.Kind == "" || p.Failure.Basis == ""):
		return errors.New("coord: a failure's pause needs the failure's kind and the basis it failed on")
	case p.Failure != nil && p.ByKind != iam.ActorSystem:
		return errors.New("coord: only the engine pauses a seat for a failure")
	case utf8.RuneCountInString(p.Reason) > MaxSeatPauseReasonRunes:
		return fmt.Errorf("coord: a seat pause's reason is past %d characters", MaxSeatPauseReasonRunes)
	case p.Failure != nil && (!plainWithin(p.Failure.Kind, maxSeatFailureWordBytes) ||
		!plainWithin(p.Failure.Basis, maxSeatFailureWordBytes)):
		return errors.New("coord: a failure's pause names a kind or basis past its bound")
	}
	return checkActor("a seat pause", p.By, p.OperatorID)
}

// MaxSeatPauseReasonRunes bounds a pause's reason, in characters: one line on
// the seat's card, which the pause tools refuse past it (internal/agent/builtin)
// and the engine's own reasons keep far inside.
const MaxSeatPauseReasonRunes = 500

// maxSeatFailureWordBytes bounds a failure's kind, a word of the engine's
// table, and its basis, a fingerprint of the settings it depends on.
const maxSeatFailureWordBytes = 128

// SeatPauseClass is the key class a pause is filed under in the positions
// register.
const SeatPauseClass = "seat_pause"

// SeatPauseKey is a seat's key in the register, by the seat's id. See
// [PositionKey] for the classes that share it and why none of them may age.
func SeatPauseKey(seat uuid.UUID) string { return DocumentKey(SeatPauseClass, seat.String()) }

// SeatPauseSeat recovers the seat's id from a key of the pause class. It
// reports false for a key of another class and for one whose name is not a
// seat id in canonical form — a key this build did not write names no seat,
// for [SeatID]'s reason.
func SeatPauseSeat(key string) (uuid.UUID, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != SeatPauseClass {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(segments[1])
	if err != nil || id == uuid.Nil || id.String() != segments[1] {
		return uuid.Nil, false
	}
	return id, true
}

// SeatPauseUpdate is one change a pause watch delivers.
type SeatPauseUpdate struct {
	// Pause is the record the seat now holds, and nil when the seat's
	// pause was lifted.
	Pause *SeatPause

	// Seat names the seat the update is about, by its id. uuid.Nil on the
	// marker.
	Seat uuid.UUID

	// Current marks the end of the records that existed when the watch
	// started; every update after it is a change. It is the only way a
	// watch can say "no seat is paused": an empty stream says nothing.
	Current bool
}

// SeatPauses is the fleet's record of which seats are paused.
//
// Every read RAISES rather than answering empty: "no record" means the seat
// takes work, so an unreadable store saying it would restart a seat stopped
// for doing damage on a two-second blip.
type SeatPauses interface {
	// SeatPause reads one seat's pause, by the seat's id.
	SeatPause(ctx context.Context, seat uuid.UUID) (SeatPause, bool, error)

	// ListSeatPauses returns every pause, ordered by seat id, so two
	// backends answer in the same order.
	ListSeatPauses(ctx context.Context) ([]SeatPause, error)

	// CreateSeatPause writes a pause for a seat that has none, returning
	// it as stored, and reports false for a seat already paused: only an
	// update at the version its caller read may change that pause.
	CreateSeatPause(ctx context.Context, p SeatPause) (SeatPause, bool, error)

	// UpdateSeatPause writes p at p.Version, reporting false when that
	// version no longer holds, including when the pause is gone.
	UpdateSeatPause(ctx context.Context, p SeatPause) (SeatPause, bool, error)

	// DeleteSeatPause lifts a pause at a version, reporting whether that
	// version still held. A version of zero never holds.
	DeleteSeatPause(ctx context.Context, seat uuid.UUID, version uint64) (bool, error)

	// WatchSeatPauses streams every pause that exists, then the
	// [SeatPauseUpdate.Current] marker, then every change after it.
	//
	// The channel closes when ctx ends or the watch fails. A CLOSED
	// CHANNEL IS NOT A RESUME: a consumer keeps what it last knew and
	// watches again, whose replay before the marker covers what it missed.
	WatchSeatPauses(ctx context.Context) (<-chan SeatPauseUpdate, error)
}
