package assistantdomain

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// The sentinels. Each is a fact a caller acts on differently, which is why they
// are separate.
var (
	// ErrNotFound reports a conversation, turn, card or proposal that does
	// not exist OR BELONGS TO SOMEBODY ELSE: a caller with no relation to a
	// conversation is told it does not exist, never that it is forbidden
	// (ADR-0053).
	ErrNotFound = errors.New("assistantdomain: not found")

	// ErrPurged reports a write to a conversation a purge or an erasure
	// destroyed for good. A surface answers it as not found.
	ErrPurged = errors.New("assistantdomain: the conversation was purged")

	// ErrErased reports a write for a person whose erasure took everything
	// they owned here.
	ErrErased = errors.New("assistantdomain: the person was erased")

	// ErrTurnRunning reports a turn opened in a conversation that already
	// has one running or parked: one at a time.
	ErrTurnRunning = errors.New("assistantdomain: the conversation has a turn " +
		"running or waiting on an approval")

	// ErrTurnOpen reports a turn opened that is already open: a retry of
	// its opening, which landed before, on this node or another. Nothing
	// was written; the caller starts nothing, since the first opening's
	// runner has the turn.
	ErrTurnOpen = errors.New("assistantdomain: the turn is already open")

	// ErrTurnMoved reports a turn move from a state the turn is no longer in:
	// a park of a turn that is not running, a resume of one that is not
	// parked, a second close.
	ErrTurnMoved = errors.New("assistantdomain: the turn is not in the state " +
		"this move needs")

	// ErrAlreadyDecided reports a proposal move decided from a state the
	// proposal has left, a second approval above all. [AlreadyDecided]
	// carries the state it is in now.
	ErrAlreadyDecided = errors.New("assistantdomain: the proposal was already decided")

	// ErrNotDue reports a retention purge of a conversation that is no
	// longer untouched since before the cutoff: used again since it was
	// listed, or gone.
	ErrNotDue = errors.New("assistantdomain: the conversation is not past its retention")

	// ErrConflict reports a write that lost its compare-and-set every round:
	// something kept changing the object.
	ErrConflict = errors.New("assistantdomain: the object kept changing under this write")

	// ErrAuditPending reports a compliance read recorded and not yet
	// applied on the node asked to open what it read: its owner could not
	// see it there yet, so nothing is opened. It is also [ErrNotFound], since
	// nothing is found until it applies; a surface tells the two apart to
	// answer "send it again" rather than "there is no such thing".
	ErrAuditPending = errors.New("assistantdomain: the compliance read has not " +
		"applied on this node")

	// ErrTooManyFiles reports a file a conversation may not keep: it
	// already holds [FilesPerConversation] of the file's origin.
	ErrTooManyFiles = errors.New("assistantdomain: the conversation keeps no more " +
		"files of this kind")

	// ErrInvalid reports a write whose input the domain refuses, naming the
	// field and what to change.
	ErrInvalid = errors.New("assistantdomain: invalid")
)

// AlreadyDecided is a proposal move refused because the proposal is no longer
// in the state the move was decided from. It wraps [ErrAlreadyDecided].
type AlreadyDecided struct {
	// State is the state the proposal is in now.
	State wire.ProposalState
}

func (e *AlreadyDecided) Error() string {
	return fmt.Sprintf("%s: it is %s now", ErrAlreadyDecided, e.State)
}

// Unwrap makes [AlreadyDecided] errors.Is-comparable with [ErrAlreadyDecided].
func (e *AlreadyDecided) Unwrap() error { return ErrAlreadyDecided }

// TurnMoved is a turn move refused because the turn is in another state. It
// wraps [ErrTurnMoved].
type TurnMoved struct {
	// State is the state the turn is in now.
	State wire.TurnState
}

func (e *TurnMoved) Error() string {
	return fmt.Sprintf("%s: it is %s", ErrTurnMoved, e.State)
}

// Unwrap makes [TurnMoved] errors.Is-comparable with [ErrTurnMoved].
func (e *TurnMoved) Unwrap() error { return ErrTurnMoved }

// invalid is an [ErrInvalid] naming what to change.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
