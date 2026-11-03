package executordomain

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// The sentinels. Each is a fact a caller acts on differently, which is why they
// are separate.
var (
	// ErrNotFound reports a turn this seat holds no row for.
	ErrNotFound = errors.New("executordomain: not found")

	// ErrTurnEnded reports a write to a turn that has ended, or whose row a
	// retention or a seat's removal took: its tombstone takes no further
	// capture and no second end. A capture refused this way is not an error
	// of the capture's own; the turn is over.
	ErrTurnEnded = errors.New("executordomain: the turn has ended")

	// ErrStaleCapture reports a capture whose (launch, capture) ordinals
	// are not newer than the ones the turn's row already holds: a late or
	// retried capture the row has moved past. Nothing was written, and the
	// newer session stays.
	ErrStaleCapture = errors.New("executordomain: a newer capture of the turn is " +
		"already recorded")

	// ErrUploadStale reports a capture whose object was minted longer than
	// objstore.RecordWithin before the capture was decided: the collector
	// may already be reclaiming it, so it is never named. Upload it again.
	ErrUploadStale = errors.New("executordomain: the session's object is too old " +
		"to name")

	// ErrOverBudget reports a capture the seat's byte budget cannot hold
	// even with every ended turn's session released: the seat's live turns
	// already hold it. Nothing was written; the turn's next launch starts
	// from the session the row already names, or fresh.
	ErrOverBudget = errors.New("executordomain: the seat's session budget cannot " +
		"hold this capture")

	// ErrNotDue reports a retention delete of a turn that is no longer past
	// the cutoff (captured again since it was listed) or no longer there.
	ErrNotDue = errors.New("executordomain: the session is not past its retention")

	// ErrUnreadable reports a session's object that cannot be read back as
	// the session its row names: gone from the store, its bytes not the
	// ones it was stored with, or sealed under a keyring key this node no
	// longer holds. A restorer reads it as NO SESSION and starts the turn
	// fresh with the account of the turns before it, never as a failed
	// launch.
	ErrUnreadable = errors.New("executordomain: the session's object cannot be read")

	// ErrSessionTooLarge reports an archive past [SessionMaxBytes]: nothing
	// is recorded, and the turn's next launch starts from the session the
	// row already names, or fresh.
	ErrSessionTooLarge = errors.New("executordomain: the session is over its size bound")

	// ErrConflict reports a write that lost its compare-and-set every round:
	// something kept changing the conversation.
	ErrConflict = errors.New("executordomain: the conversation kept changing under " +
		"this write")

	// ErrInvalid reports a write whose input the domain refuses, naming the
	// field and what to change.
	ErrInvalid = errors.New("executordomain: invalid")

	// ErrIncomplete reports a listing this node's rows cannot answer whole:
	// it holds a record of the log it could not decode, whose rows may be
	// the very ones asked about. Another node, or this one once a build that
	// reads the record has applied it, answers.
	ErrIncomplete = errors.New("executordomain: this node may be missing rows the " +
		"answer needs")
)

// invalid is an [ErrInvalid] naming what to change.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// checkText refuses a text field outside its bounds.
func checkText(field, value string, maxBytes int) error {
	switch {
	case !utf8.ValidString(value):
		return invalid("%s is not valid UTF-8", field)
	case len(value) > maxBytes:
		return invalid("%s is %d bytes and the maximum is %d", field, len(value), maxBytes)
	}
	return nil
}
