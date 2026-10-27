package assistant

import (
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/live"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/events/types"
)

// The sentinels the HTTP surface maps to the engine's codes
// (internal/api/assistantapi). Each is a different thing a caller acts on,
// which is why they are separate.
var (
	// ErrNotFound reports a conversation, turn or card that does not exist
	// OR IS SOMEBODY ELSE'S: a caller with no relation is told it does not
	// exist (ADR-0053).
	ErrNotFound = errors.New("assistant: not found")
	// ErrTurnRunning reports a question asked in a conversation whose turn
	// is running (one at a time; a turn waiting on an approval is asked
	// past, and closes superseded), or a decision handed a turn that no
	// longer waits on it.
	ErrTurnRunning = errors.New("assistant: the conversation has a turn running")
	// ErrBusy reports a person past their running turns or their turn rate.
	// [Busy] carries when to try again.
	ErrBusy = errors.New("assistant: too many turns at once")
	// ErrUnavailable reports an assistant this node cannot run now: every
	// model in the chain failing, the store unreachable, or the turn's
	// runner on a node this one cannot reach. [Unavailable] carries when to
	// try again where that is known.
	ErrUnavailable = errors.New("assistant: unavailable")
	// ErrNotAvailable reports an assistant this person may not use here:
	// [NotAvailable] carries the reason `/viewer` gives.
	ErrNotAvailable = errors.New("assistant: not available to this person")
	// ErrBudget reports a question refused before any round because a
	// window the company's budget caps has no room. [BudgetRefused] carries
	// the window.
	ErrBudget = errors.New("assistant: the token budget refuses the turn")
	// ErrInvalid reports a request the assistant refuses as written, naming
	// the field.
	ErrInvalid = errors.New("assistant: invalid")
	// ErrCaughtUp reports a stream with nothing to send: the turn's segment
	// has ended and the client resumed after its last frame. The one
	// sentinel the local log and the fleet's follow both answer
	// ([live.ErrCaughtUp]), named here beside the others the surface maps.
	ErrCaughtUp = live.ErrCaughtUp

	// The proposal outcomes, answered by the approval path
	// (internal/assistant/proposal and its routes).
	ErrProposalDecided = errors.New("assistant: the proposal was already decided")
	ErrProposalExpired = errors.New("assistant: the proposal expired")
	ErrDigestMismatch  = errors.New("assistant: the approval's digest is not the proposal's")
	ErrStale           = errors.New("assistant: the object moved since the change was proposed")
	ErrActionClaimed   = errors.New("assistant: another tab already claimed the page action")
)

// Busy is a turn refused for one person's load. It wraps [ErrBusy].
type Busy struct {
	RetryAfter time.Duration
	Reason     string
}

func (e *Busy) Error() string { return fmt.Sprintf("%s: %s", ErrBusy, e.Reason) }

// Unwrap makes [Busy] errors.Is-comparable with [ErrBusy].
func (e *Busy) Unwrap() error { return ErrBusy }

// Unavailable is an assistant this node cannot run now. It wraps
// [ErrUnavailable]; RetryAfter is zero where no wait is known to clear it.
type Unavailable struct {
	RetryAfter time.Duration
	Cause      error
}

func (e *Unavailable) Error() string { return fmt.Sprintf("%s: %v", ErrUnavailable, e.Cause) }

// Unwrap makes [Unavailable] errors.Is-comparable with [ErrUnavailable] and
// with its cause.
func (e *Unavailable) Unwrap() []error { return []error{ErrUnavailable, e.Cause} }

// NotAvailable is an assistant refused to this person. It wraps
// [ErrNotAvailable].
type NotAvailable struct {
	Reason wire.ViewerReason
	Detail string
}

func (e *NotAvailable) Error() string {
	return fmt.Sprintf("%s (%s): %s", ErrNotAvailable, e.Reason, e.Detail)
}

// Unwrap makes [NotAvailable] errors.Is-comparable with [ErrNotAvailable].
func (e *NotAvailable) Unwrap() error { return ErrNotAvailable }

// BudgetRefused is a turn the company's windows refuse before its first
// round. It wraps [ErrBudget].
type BudgetRefused struct {
	Scope    string
	Window   string
	Used     int
	Limit    int
	ResetsAt time.Time
}

func (e *BudgetRefused) Error() string {
	return fmt.Sprintf("%s: the %s window %s is at %d of %d, resets %s", ErrBudget, e.Scope,
		e.Window, e.Used, e.Limit, e.ResetsAt.UTC().Format(time.RFC3339))
}

// Unwrap makes [BudgetRefused] errors.Is-comparable with [ErrBudget].
func (e *BudgetRefused) Unwrap() error { return ErrBudget }

// Remedy is where the ceiling that refused the turn is changed, or the zero
// [caveat.Remedy] where the refusal names no scope it knows.
func (e *BudgetRefused) Remedy() caveat.Remedy { return budgetRemedy(e.Scope) }

// budgetRemedy is the setting whose ceiling a refusal by scope was, for every
// scope an assistant turn's meter refuses by: the company's own, or a
// person's ([PersonBudgetScope]). The brief's cap ([BriefBudgetScope]) is a
// constant of the turn rather than a setting, and names none.
func budgetRemedy(scope string) caveat.Remedy {
	switch scope {
	case string(types.BudgetScopeOrg):
		return caveat.CompanyTokenBudget()
	case PersonBudgetScope:
		return caveat.AssistantTokenBudget()
	}
	return caveat.Remedy{}
}

// invalid is an [ErrInvalid] naming what to change.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
