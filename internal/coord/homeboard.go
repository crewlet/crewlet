package coord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A person's Home board: the widgets they placed on the dashboard's Home and
// how they arranged them, kept here so it follows them to every browser.
//
// Keyed by the human seat [iam.RecordOwner] names and passed with it, as chat
// read state is (package doc, "A person's own record here is their SEAT's").
// The engine never reads inside the board: the dashboard owns its shape and
// validates it as untrusted input on every read. A class of the ageless
// positions register, since one save per "Done" adds nothing to its load; it
// leaves only by the reconcile that forgets chat read state (chatread.go).

// MaxHomeBoardBytes is the most one saved board may encode to, re-encoded
// without HTML escaping. Sixty widgets at the dashboard's own limits (notes of
// 2,000 characters, twelve links a list) in plain text encode to about 440 KB,
// and it sits inside the act route's body bound (operator.MaxActBody).
const MaxHomeBoardBytes = 512 << 10

// MaxHomeBoardRecordBytes bounds one board's record as stored: the board, kept
// in its own bytes, and 256 for its two instants and field names, which take
// 104. A save decides on a read of it, which crosses a leaf's link at the
// slowest pace in 20 s.
const MaxHomeBoardRecordBytes = MaxHomeBoardBytes + 256

// ErrInvalidHomeBoard marks a board refused for what it is (not a JSON object,
// no widget list, past [MaxHomeBoardBytes]), refused the same way every time.
var ErrInvalidHomeBoard = errors.New("coord: a Home board was refused")

// CheckHomeBoard is nil for a board a save may store: a JSON object whose
// `widgets` is an array, of at most [MaxHomeBoardBytes]. Both backends refuse
// a save by it, so no store holds a board the tool would have refused.
func CheckHomeBoard(board json.RawMessage) error {
	if len(board) > MaxHomeBoardBytes {
		return fmt.Errorf("%w: the board encodes to %d bytes, past the %d one person's "+
			"Home keeps; remove widgets or shorten their text", ErrInvalidHomeBoard,
			len(board), MaxHomeBoardBytes)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(board, &doc); err != nil || doc == nil {
		return fmt.Errorf("%w: the board is a JSON object, {\"widgets\": [...]}",
			ErrInvalidHomeBoard)
	}
	if widgets := bytes.TrimSpace(doc["widgets"]); len(widgets) == 0 || widgets[0] != '[' {
		return fmt.Errorf("%w: the board's `widgets` is an array, empty for a bare Home",
			ErrInvalidHomeBoard)
	}
	return nil
}

// HomeBoardSentence is a refused board as its sender reads it, without the
// sentinel's prefix; any other error is its whole message ([ReadStateSentence]).
func HomeBoardSentence(err error) string {
	if rest, ok := strings.CutPrefix(err.Error(), ErrInvalidHomeBoard.Error()+": "); ok &&
		errors.Is(err, ErrInvalidHomeBoard) {
		return rest
	}
	return err.Error()
}

// HomeBoard is one person's Home board as the store keeps it.
type HomeBoard struct {
	// Board is the document exactly as the dashboard saved it, a JSON object.
	Board json.RawMessage `json:"board"`

	// UpdatedAt is when it was last saved: diagnostic, nothing branches on it.
	UpdatedAt time.Time `json:"updated_at,omitzero"`

	// AbsentSince is when the reconcile first found the seat missing from the
	// company, zero while the company has it ([ChatReadState.AbsentSince]).
	AbsentSince time.Time `json:"absent_since,omitzero"`
}

// WithAbsence is the board with its absence stamp set to since, and whether
// that changed anything: [ChatReadState.WithAbsence]'s rule.
func (b HomeBoard) WithAbsence(since time.Time) (HomeBoard, bool) {
	stamp, changed := stampAbsence(b.AbsentSince, since)
	b.AbsentSince = stamp
	return b, changed
}

// AbsentBy reports whether the board was stamped absent at or before cutoff:
// the one condition under which [HomeBoards.ForgetHomeBoard] removes it.
func (b HomeBoard) AbsentBy(cutoff time.Time) bool { return absentBy(b.AbsentSince, cutoff) }

// HomeBoardOwner is one seat holding a board, as the reconcile lists them.
type HomeBoardOwner struct {
	Handle string

	// AbsentSince is [HomeBoard.AbsentSince].
	AbsentSince time.Time
}

// HomeBoards is the company's per-person Home boards, one record per human
// seat (the comments atop homeboard.go).
type HomeBoards interface {
	// HomeBoard is a seat's board and its revision, false where none is stored.
	HomeBoard(ctx context.Context, handle string) (HomeBoard, uint64, bool, error)

	// SaveHomeBoard replaces a seat's board if it is still at revision (zero:
	// none is stored), answering the new revision, or false where it moved.
	// It keeps the absence stamp: only the reconcile observes absence. A board
	// [CheckHomeBoard] refuses is [ErrInvalidHomeBoard]; [ErrWriteUnknown]
	// marks a write that may have landed; any other error changed nothing.
	SaveHomeBoard(ctx context.Context, handle string, board json.RawMessage, at time.Time,
		revision uint64) (uint64, bool, error)

	// SetHomeBoardAbsence is [ChatReads.SetChatReadAbsence] for a board.
	SetHomeBoardAbsence(ctx context.Context, handle string, since time.Time) (bool, error)

	// ForgetHomeBoard is [ChatReads.ForgetChatRead] for a board.
	ForgetHomeBoard(ctx context.Context, handle string, absentBy time.Time) (bool, error)

	// HomeBoardOwners is every seat holding a board, by handle, with its
	// absence stamp. It raises rather than answer short ([ChatReads.ChatReaders]).
	HomeBoardOwners(ctx context.Context) ([]HomeBoardOwner, error)
}

// HomeBoardClass is the key class a board is filed under in the positions
// register, which no clock ages.
const HomeBoardClass = "home_board"

// HomeBoardKey is one seat's key.
func HomeBoardKey(handle string) string { return DocumentKey(HomeBoardClass, handle) }

// HomeBoardFilter selects every seat's board.
func HomeBoardFilter() string { return DocumentFilter(HomeBoardClass) }

// HomeBoardHandle recovers the seat's handle from a key, false for another class.
func HomeBoardHandle(key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != HomeBoardClass || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}

// stampAbsence is the one rule for a personal record's absence stamp: a
// non-zero since keeps an earlier stamp, since the grace runs from the FIRST
// sweep that saw the seat missing, and the zero instant clears it.
func stampAbsence(held, since time.Time) (time.Time, bool) {
	switch {
	case since.IsZero() && held.IsZero():
		return held, false
	case since.IsZero():
		return time.Time{}, true
	case !held.IsZero():
		return held, false
	}
	return since.UTC(), true
}

// absentBy reports whether a stamp was set at or before cutoff.
func absentBy(stamp, cutoff time.Time) bool {
	return !stamp.IsZero() && !stamp.After(cutoff)
}
