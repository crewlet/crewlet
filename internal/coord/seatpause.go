package coord

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// A SEAT PAUSE is a person's decision that one seat stops taking work until
// somebody lifts it.
//
// # Who has to agree on it
//
// The whole company, now — the third answer of the four. A person pauses a
// seat through whichever node serves their request, the seat's mail is held by
// whichever node holds the seat, and placement can move the seat to a third
// node while it stays paused. So the decision cannot live in any one node's
// process or database: every node reads it, the node holding the seat acts on
// it, and a node that acquires the seat later has to find it without having
// seen the gesture. It is one record per seat in the coordination store, and
// every node keeps a watched copy of all of them (internal/engine/seatpause.go).
//
// # No age
//
// It lives in the positions register, the one bucket with no retention, under
// its own key class. An expiring pause would be a resume nobody chose: a seat
// a person stopped because it was doing damage would start again on a timer
// nobody set. What ends a pause is a person resuming it, or the seat leaving
// the company (the apply that removes it clears the record).
//
// # Keyed on the seat's IDENTITY
//
// The record is filed under the seat's agent id, derived from its handle
// (ADR-0013), as the inbox the hold sits on is.
//
// # Compare-and-set
//
// Two people can pause and resume one seat at once, and the one outcome that
// must not happen is a lost update — a resume overwritten by a pause that read
// the record before it. So a pause is a CREATE (the first writer wins, a
// second pause of a paused seat is told it was already paused), an amendment
// is an update at the version its writer read, and a resume is a delete at
// that version. The writer that WON is the one that announces it, which is
// what keeps `seat_paused` and `seat_resumed` exactly once per change however
// many callers raced.

// SeatPause is one paused seat, as the coordination store holds it.
type SeatPause struct {
	// Seat is the paused seat's id — its agent id — and the record's key.
	Seat uuid.UUID `json:"seat"`

	// By is who paused it, ByKind what sort of author that is, and
	// OperatorID the credential they acted through: internal/iam's
	// [iam.Actor] for the request, exactly as every other write the same
	// principal makes records it. A person bound to a seat pauses AS that
	// seat, kind human, with their credential beside it; a credential
	// nobody is bound through pauses under its own login, kind operator.
	// The credential is what an audit asks about, the author is who a
	// screen names.
	By         string        `json:"by"`
	ByKind     iam.ActorKind `json:"by_kind"`
	OperatorID string        `json:"operator_id,omitempty"`

	// Reason is why, in the pauser's own words. Optional.
	Reason string `json:"reason,omitempty"`

	// StopRunning asks the node running the seat to end the turn it is on
	// at that turn's next round boundary, rather than letting it finish.
	// Without it a pause only stops NEW work, and the turn in flight runs
	// to its end.
	StopRunning bool `json:"stop_running,omitempty"`

	// At is when the seat was paused.
	At time.Time `json:"at"`

	// Version is the store's version of the record as it was read. OPAQUE,
	// like [MailboxRecord.Version]: pass back exactly what a read or a
	// write handed you. Not on the wire; ignored by CreateSeatPause.
	Version uint64 `json:"-"`
}

// Validate reports why a pause cannot be written.
//
// A kind this build cannot name is refused on the way IN, and only there: a
// record read back keeps whatever kind it carries ([iam.ActorKind.Valid] is
// how a reader tells), but this build writes nothing it could not itself
// have decided.
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
	}
	return nil
}

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
	// started: every update before it is one of those, and every update
	// after it is a change. A consumer builds its first complete answer
	// out of what precedes it, which is the only way a watch can say "no
	// seat is paused" — an empty stream says nothing at all.
	Current bool
}

// SeatPauses is the fleet's record of which seats a person has paused.
//
// # RAISES rather than answering empty, on every read
//
// "No record" means the seat takes work. A store that could not be read must
// never be able to say that: a seat somebody stopped because it was doing
// damage would start again on a two-second blip.
type SeatPauses interface {
	// SeatPause reads one seat's pause, by the seat's id.
	SeatPause(ctx context.Context, seat uuid.UUID) (SeatPause, bool, error)

	// ListSeatPauses returns every pause, ordered by seat id, so two
	// backends answer in the same order.
	ListSeatPauses(ctx context.Context) ([]SeatPause, error)

	// CreateSeatPause writes a pause for a seat that has none, returning
	// it as stored. A seat that already has one is left alone and reports
	// false: the existing pause belongs to whoever took it, and only an
	// update conditioned on the version its caller read may change it.
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
	// The channel is closed when ctx ends or the watch fails. A CLOSED
	// CHANNEL IS NOT A RESUME: the watcher has stopped hearing, and a
	// consumer keeps what it last knew and watches again — the records
	// that exist then are delivered again before the next marker, so a
	// change it missed while it was not listening is not lost.
	WatchSeatPauses(ctx context.Context) (<-chan SeatPauseUpdate, error)
}
