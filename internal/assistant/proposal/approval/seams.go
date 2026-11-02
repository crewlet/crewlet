package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// Store is the slice of internal/assistantdomain the approval path reads and
// writes: one of the owner's proposals, the proposals of one turn, and one
// arbitrated move. [Domain] is the production implementation.
type Store interface {
	Proposal(ctx context.Context, owner, id uuid.UUID) (assistantdomain.Proposal, error)
	TurnProposals(ctx context.Context, owner, conversation, turn uuid.UUID) ([]assistantdomain.Proposal, error)
	Move(ctx context.Context, c assistantdomain.Call, id uuid.UUID, m assistantdomain.ProposalMove) (Written, error)
}

// Written is what a recorded move answered: the state log's three-valued
// outcome and whether the call was a retry of an operation already recorded.
type Written struct {
	// Outcome is `applied`, `pending` or `unknown`. Applied and pending
	// both mean the broker accepted the record; unknown means nobody can
	// say, and nothing is built on it.
	Outcome string
	// Collapsed says the operation had already been recorded under this
	// op id, so this call wrote nothing and decided nothing.
	Collapsed bool
}

// landed reports whether the record is known to be in the log.
func (w Written) landed() bool { return w.Outcome == "applied" || w.Outcome == "pending" }

// Exec is the one request an exec grant is derived for.
type Exec struct {
	Conversation, Turn, Proposal uuid.UUID
	// Act is the verb the request executes as, for the grant's audit
	// reference.
	Act     string
	Request exec.Request
}

// Grants derives and ends assistant grants from the deciding request. The API
// implements it over internal/api/auth's Delegations, reading the person and
// their session from ctx: a grant is never derived from anything else.
type Grants interface {
	// Read is a read grant for the resumed turn and for the revision
	// re-read, derived from this request.
	Read(ctx context.Context, conversation, turn uuid.UUID) (exec.Grant, error)
	// Exec is a single-use exec grant bound to x's request, derived from
	// this request.
	Exec(ctx context.Context, x Exec) (exec.Grant, error)
	// Revoke ends a grant.
	Revoke(grant uuid.UUID)
}

// Resumption is what became of a parked turn's proposal, for the turn to carry
// on from.
type Resumption struct {
	Owner, Conversation, Turn, Proposal uuid.UUID

	// Read is the resumed turn's read grant, derived from the deciding
	// request: the turn that resumes acts on the click that resumed it.
	Read exec.Grant

	// State is applied, failed, declined or stale.
	State   wire.ProposalState
	Receipt *wire.Receipt
	Failure *wire.Failure

	// Answer is the executed request's own answer, verbatim, for a server
	// write that was sent; nil otherwise.
	Answer json.RawMessage

	// ClientReported marks a page action's outcome, which the page stated
	// and the engine cannot verify: the model reads it under a
	// client-reported trust label, never as engine-authored.
	ClientReported bool

	// Card is the proposal card at its state now, for the stream.
	Card wire.Card

	// Page is the screen the decision was made from, as the deciding
	// request carried it: the actions it accepts and its draft as they are
	// after the decision. Nil when the request carried none.
	Page *wire.PageContext

	// Invalidate is where a server write landed, for the
	// `context.invalidate` action the open screens raise their read floor
	// by; nil where nothing landed.
	Invalidate *wire.Invalidate
}

// Emission is frames for a turn that stays parked: the proposal card at its
// state now, and for an approved page action the action the page applies.
type Emission struct {
	Card   *wire.Card
	Action *wire.ActionEvent
}

// Turns is the slice of the assistant's service a decision drives. The
// service owns every goroutine a turn runs on, so Resume hands the turn over
// and answers where the client reads it; it never runs the turn on ctx.
type Turns interface {
	// Resume takes the parked turn back up on this node with what became
	// of its proposal.
	Resume(ctx context.Context, r Resumption) (wire.TurnAccepted, error)
	// Emit appends frames to a turn that stays parked, and answers where
	// the client reads them.
	Emit(ctx context.Context, owner, conversation, turn uuid.UUID, e Emission) (wire.TurnAccepted, error)
	// Lapse ends the parked turn as expired where none of its proposals
	// can still move it, now that one of them expired.
	Lapse(ctx context.Context, owner, conversation, turn uuid.UUID) error
	// Carries reports whether the deciding person may still have the
	// parked turn carry on: false where they no longer hold an app its loop
	// read through, so what it read may not be handed back to the model
	// for them.
	Carries(ctx context.Context, owner, conversation, turn uuid.UUID) (bool, error)
}

// Deps is what an [Approver] is built from.
type Deps struct {
	Store      Store
	Grants     Grants
	Dispatcher exec.Dispatcher
	Turns      Turns

	// Revisions re-reads a proposal's source revision as the person.
	Revisions proposal.Revisions

	// StepUp reports whether a verb's row asks a recent proof, so an
	// approval asks one even of a proposal recorded without it.
	StepUp func(act string) bool

	// StepOp derives one step's operation id from a request's operation
	// (internal/statelog.StepOpID). Injected because nothing under
	// internal/assistant imports the state log.
	StepOp func(opID string, steps ...string) string

	// DomainOf names the state-log domain whose records live on a stream,
	// or "" for a stream no domain owns: how a write's landing position
	// (`<stream>@<generation>:<seq>`) names the log a screen's read waits on
	// (internal/engine.LogDomain). Injected for StepOp's reason.
	DomainOf func(stream string) string

	// Now is the clock; nil is the wall clock in UTC.
	Now func() time.Time
}

// Approver carries decisions out.
type Approver struct {
	d Deps
}

// New builds an approver, refusing a missing seam by name.
func New(d Deps) (*Approver, error) {
	switch {
	case d.Store == nil:
		return nil, errors.New("approval: a store is required: every decision is a record")
	case d.Grants == nil:
		return nil, errors.New("approval: grants are required: a write is sent as the person")
	case d.Dispatcher == nil:
		return nil, errors.New("approval: a dispatcher is required: a write is a request")
	case d.Turns == nil:
		return nil, errors.New("approval: the turns are required: a decision resumes its turn")
	case d.Revisions == nil:
		return nil, errors.New("approval: a revision reader is required: a stale " +
			"proposal is never executed")
	case d.StepUp == nil:
		return nil, errors.New("approval: the step-up rule is required")
	case d.StepOp == nil:
		return nil, errors.New("approval: the step operation id derivation is required")
	case d.DomainOf == nil:
		return nil, errors.New("approval: the stream's domain is required: an applied " +
			"write tells open screens which log moved")
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Approver{d: d}, nil
}

// The sentinels, each a different answer the API gives.
var (
	// ErrNotFound is a proposal that does not exist or is somebody else's.
	ErrNotFound = errors.New("approval: no such proposal")
	// ErrAlreadyDecided is a proposal no longer waiting; [Decided] carries
	// its state now.
	ErrAlreadyDecided = errors.New("approval: the proposal was already decided")
	// ErrExpired is a proposal nobody decided before its expiry.
	ErrExpired = errors.New("approval: the proposal expired")
	// ErrStale is a proposal whose object moved since its preview.
	ErrStale = errors.New("approval: the proposal's object changed since it was shown")
	// ErrDigestMismatch is an approval of something the card did not show.
	ErrDigestMismatch = proposal.ErrDigestMismatch
	// ErrStepUp is an approval that needs the person to confirm it is
	// them first; nothing was recorded.
	ErrStepUp = errors.New("approval: confirm it is you, then approve again")
	// ErrActionClaimed is a page action another tab already claimed.
	ErrActionClaimed = errors.New("approval: the page action was already claimed")
	// ErrUnavailable is a decision this node could not carry out or
	// confirm; [Unconfirmed] carries the operation to retry under.
	ErrUnavailable = errors.New("approval: the decision could not be confirmed")
	// ErrNotAPerson is a decision with no resolved person behind it.
	ErrNotAPerson = errors.New("approval: only the person a proposal is for decides it")
)

// Decided is [ErrAlreadyDecided] with the state the proposal is in now.
type Decided struct{ State wire.ProposalState }

func (e *Decided) Error() string { return fmt.Sprintf("%s: it is %s", ErrAlreadyDecided, e.State) }

// Unwrap makes Decided errors.Is-comparable with [ErrAlreadyDecided].
func (e *Decided) Unwrap() error { return ErrAlreadyDecided }

// Unconfirmed is [ErrUnavailable] naming the operation a retry sends again.
type Unconfirmed struct {
	OpID  string
	Cause error
}

func (e *Unconfirmed) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s (op %s): %v", ErrUnavailable, e.OpID, e.Cause)
	}
	return fmt.Sprintf("%s (op %s)", ErrUnavailable, e.OpID)
}

// Unwrap makes Unconfirmed errors.Is-comparable with [ErrUnavailable].
func (e *Unconfirmed) Unwrap() error { return ErrUnavailable }
