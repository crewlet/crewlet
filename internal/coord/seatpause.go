package coord

import (
	"context"
	"errors"
	"time"
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
	// Handle is the paused seat, and the record's key.
	Handle string `json:"handle"`

	// By is who paused it — the author of the write: for a person acting
	// through a credential, the token's own name, exactly as a tracker
	// write made through the same credential names it.
	By string `json:"by"`

	// OperatorID is the credential the pause was made under, and Seat the
	// chart seat that credential is bound to — the PERSON — empty for a
	// credential nobody bound. Two facts, as on every operator write: the
	// credential is what an audit asks about, the person is who a screen
	// names.
	OperatorID string `json:"operator_id,omitempty"`
	Seat       string `json:"seat,omitempty"`

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
func (p SeatPause) Validate() error {
	switch {
	case p.Handle == "":
		return errors.New("coord: a seat pause needs the handle of the seat it pauses")
	case p.By == "":
		return errors.New("coord: a seat pause needs who paused it: a pause nobody " +
			"can be named for is one nobody can be asked about")
	case p.At.IsZero():
		return errors.New("coord: a seat pause needs the instant it was taken")
	}
	return nil
}

// SeatPauseClass is the key class a pause is filed under in the positions
// register.
const SeatPauseClass = "seat_pause"

// SeatPauseKey is a seat's key in the register. See [PositionKey] for the
// classes that share it and why none of them may age.
func SeatPauseKey(handle string) string { return DocumentKey(SeatPauseClass, handle) }

// SeatPauseUpdate is one change a pause watch delivers.
type SeatPauseUpdate struct {
	// Pause is the record the seat now holds, and nil when the seat's
	// pause was lifted.
	Pause *SeatPause

	// Handle names the seat the update is about. Empty on the marker.
	Handle string

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
	// SeatPause reads one seat's pause.
	SeatPause(ctx context.Context, handle string) (SeatPause, bool, error)

	// ListSeatPauses returns every pause, ordered by handle.
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
	DeleteSeatPause(ctx context.Context, handle string, version uint64) (bool, error)

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
