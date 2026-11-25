package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A person's own Ask Crewlet instructions: how they want its answers written,
// applied on top of the company's (the Tier B `assistant.instructions`).
//
// Keyed by the human seat [iam.RecordOwner] names and passed with it, as a Home
// board is (package doc, "A person's own record here is their SEAT's"). The
// store keeps the document in its own bytes: its shape and rules are
// config.AssistantInstructions', which the one tool that saves it checks first
// and every reader checks again.

// MaxAssistantInstructionsBytes is the most one person's instructions may
// encode to: a text of config.AssistantInstructionsMaxRunes at four bytes each,
// with room for its escapes, its two choices and their names.
const MaxAssistantInstructionsBytes = 8 << 10

// MaxAssistantInstructionsRecordBytes bounds the record as stored: the
// instructions and 256 for its two instants and field names.
const MaxAssistantInstructionsRecordBytes = MaxAssistantInstructionsBytes + 256

// ErrInvalidAssistantInstructions marks instructions refused for their shape
// (not a JSON object, past [MaxAssistantInstructionsBytes]).
var ErrInvalidAssistantInstructions = errors.New("coord: Ask Crewlet instructions were refused")

// CheckAssistantInstructions is nil for instructions a save may store: a JSON
// object of at most [MaxAssistantInstructionsBytes]. Both backends refuse a
// save by it.
func CheckAssistantInstructions(doc json.RawMessage) error {
	if len(doc) > MaxAssistantInstructionsBytes {
		return fmt.Errorf("%w: they encode to %d bytes, past the %d one person's keep; "+
			"shorten the text", ErrInvalidAssistantInstructions, len(doc), MaxAssistantInstructionsBytes)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(doc, &fields); err != nil || fields == nil {
		return fmt.Errorf("%w: they are a JSON object, {\"length\": ..., \"format\": ..., \"text\": ...}",
			ErrInvalidAssistantInstructions)
	}
	return nil
}

// AssistantInstructionsSentence is a refused save as its sender reads it,
// without the sentinel's prefix ([HomeBoardSentence]'s rule).
func AssistantInstructionsSentence(err error) string {
	if rest, ok := strings.CutPrefix(err.Error(), ErrInvalidAssistantInstructions.Error()+": "); ok &&
		errors.Is(err, ErrInvalidAssistantInstructions) {
		return rest
	}
	return err.Error()
}

// SeatInstructions is one person's instructions as the store keeps them.
type SeatInstructions struct {
	// Instructions are the document exactly as saved, a JSON object.
	Instructions json.RawMessage `json:"instructions"`

	// UpdatedAt is when they were last saved: diagnostic, nothing branches on it.
	UpdatedAt time.Time `json:"updated_at,omitzero"`

	// AbsentSince is [HomeBoard.AbsentSince]'s stamp for this record.
	AbsentSince time.Time `json:"absent_since,omitzero"`
}

// WithAbsence is [HomeBoard.WithAbsence] for instructions.
func (s SeatInstructions) WithAbsence(since time.Time) (SeatInstructions, bool) {
	stamp, changed := stampAbsence(s.AbsentSince, since)
	s.AbsentSince = stamp
	return s, changed
}

// AbsentBy is [HomeBoard.AbsentBy] for instructions.
func (s SeatInstructions) AbsentBy(cutoff time.Time) bool { return absentBy(s.AbsentSince, cutoff) }

// AssistantInstructions is every person's own Ask Crewlet instructions, one
// record per human seat, with [HomeBoards]' contract method for method.
type AssistantInstructions interface {
	// SeatInstructions are a seat's instructions and their revision, false
	// where none are stored.
	SeatInstructions(ctx context.Context, handle string) (SeatInstructions, uint64, bool, error)

	// SaveSeatInstructions is [HomeBoards.SaveHomeBoard] for instructions,
	// refusing what [CheckAssistantInstructions] refuses.
	SaveSeatInstructions(ctx context.Context, handle string, doc json.RawMessage, at time.Time,
		revision uint64) (uint64, bool, error)

	// SetSeatInstructionsAbsence is [HomeBoards.SetHomeBoardAbsence].
	SetSeatInstructionsAbsence(ctx context.Context, handle string, since time.Time) (bool, error)

	// ForgetSeatInstructions is [HomeBoards.ForgetHomeBoard].
	ForgetSeatInstructions(ctx context.Context, handle string, absentBy time.Time) (bool, error)

	// SeatInstructionsOwners is [HomeBoards.HomeBoardOwners].
	SeatInstructionsOwners(ctx context.Context) ([]SeatInstructionsOwner, error)
}

// SeatInstructionsOwner is one seat holding instructions, as the reconcile
// lists them.
type SeatInstructionsOwner struct {
	Handle      string
	AbsentSince time.Time
}

// AssistantInstructionsClass is the key class instructions are filed under in
// the positions register, which no clock ages.
const AssistantInstructionsClass = "assistant_instructions"

// AssistantInstructionsKey is one seat's key.
func AssistantInstructionsKey(handle string) string {
	return DocumentKey(AssistantInstructionsClass, handle)
}

// AssistantInstructionsFilter selects every seat's instructions.
func AssistantInstructionsFilter() string { return DocumentFilter(AssistantInstructionsClass) }

// AssistantInstructionsHandle recovers the seat's handle from a key, false for
// another class.
func AssistantInstructionsHandle(key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != AssistantInstructionsClass || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}
