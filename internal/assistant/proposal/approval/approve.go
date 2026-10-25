package approval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
)

var log = slog.Default().With("component", "assistant_approval")

// Approval is one approval request: the proposal it names, the digest it
// echoes, and the request's own operation, from which every record it writes
// derives its id.
type Approval struct {
	Proposal uuid.UUID
	Digest   string
	// Key is the approving request's operation: its Idempotency-Key as
	// internal/api/opkey scoped it to the person.
	Key string
}

// decider is one decision's caller: the person, as every record they write
// names them, and the request's operation.
type decider struct {
	person iam.Principal
	by     iam.Actor
	key    string
	step   func(string, ...string) string
}

// call is the record caller for one step of the decision.
func (d decider) call(step string) assistantdomain.Call {
	return assistantdomain.Call{Owner: d.person.ID, By: d.by, OpID: d.step(d.key, step)}
}

// deciderOf is the person the request resolved, and nobody else.
func (a *Approver) deciderOf(ctx context.Context, key string) (decider, error) {
	p, how := iam.From(ctx)
	if how != iam.Resolved || p.Kind != iam.KindPerson || p.ID == uuid.Nil {
		return decider{}, ErrNotAPerson
	}
	if _, assisted := p.Assisted(); assisted {
		// NEVER THROUGH THE ASSISTANT: the guard already refuses
		// `/assistant/` to a grant, and this is the second lock on the one
		// gesture the design keeps from it.
		return decider{}, ErrNotAPerson
	}
	if key == "" {
		return decider{}, fmt.Errorf("%w: a decision names its operation", ErrUnavailable)
	}
	return decider{person: p, by: iam.ActorFor(p), key: key, step: a.d.StepOp}, nil
}

// load is one of the owner's proposals.
func (a *Approver) load(ctx context.Context, owner, id uuid.UUID) (assistantdomain.Proposal, error) {
	p, err := a.d.Store.Proposal(ctx, owner, id)
	switch {
	case errors.Is(err, assistantdomain.ErrNotFound), errors.Is(err, assistantdomain.ErrPurged):
		return assistantdomain.Proposal{}, ErrNotFound
	case err != nil:
		return assistantdomain.Proposal{}, &Unconfirmed{Cause: err}
	}
	return p, nil
}

// move records one move, answering [Decided] where the proposal left the state
// it was decided from and [Unconfirmed] where nobody can say it landed.
func (a *Approver) move(ctx context.Context, c assistantdomain.Call, id uuid.UUID,
	m assistantdomain.ProposalMove) (Written, error) {

	if !proposal.Allowed(m.From, m.To) {
		return Written{}, fmt.Errorf("approval: %w: %s to %s", proposal.ErrIllegalMove, m.From, m.To)
	}
	w, err := a.d.Store.Move(ctx, c, id, m)
	var decided *assistantdomain.AlreadyDecided
	switch {
	case errors.As(err, &decided):
		return Written{}, &Decided{State: decided.State}
	case errors.Is(err, assistantdomain.ErrNotFound), errors.Is(err, assistantdomain.ErrPurged):
		return Written{}, ErrNotFound
	case err != nil:
		return Written{}, &Unconfirmed{OpID: c.OpID, Cause: err}
	case !w.landed():
		return Written{}, &Unconfirmed{OpID: c.OpID}
	}
	return w, nil
}

// Approve carries out the person's approval of one of their proposals.
//
// The answer is the proposal's state after the decision: applied, failed or
// unknown for a server write (sent within this request), approved for a page
// action (sent to the page), and where the resumed turn streams from.
func (a *Approver) Approve(ctx context.Context, ap Approval) (wire.ProposalAnswer, error) {
	d, err := a.deciderOf(ctx, ap.Key)
	if err != nil {
		return wire.ProposalAnswer{}, err
	}
	p, err := a.load(ctx, d.person.ID, ap.Proposal)
	if err != nil {
		return wire.ProposalAnswer{}, err
	}
	now := a.d.Now()
	switch state := proposal.StateNow(p, now); {
	case state == wire.ProposalExpired && p.State == wire.ProposalProposed:
		a.expire(ctx, d, p)
		return wire.ProposalAnswer{}, ErrExpired
	case state == wire.ProposalUnknown:
		return a.retry(ctx, d, p, ap.Digest)
	case state != wire.ProposalProposed:
		return a.repeat(ctx, d, p, state)
	}
	if err := proposal.Verify(p, ap.Digest); err != nil {
		return wire.ProposalAnswer{}, err
	}
	// THE STEP-UP IS ASKED OF THIS REQUEST, BEFORE ANYTHING IS RECORDED, so
	// a refusal leaves the proposal waiting and the dashboard's step-up
	// dialog replays the same approval. Bound to the person, never to the
	// session: confirming rotates the session, and the proposal is theirs.
	if (p.StepUp || a.d.StepUp(p.Act)) && !d.person.Proved(iam.RecencyStepUp, now) {
		return wire.ProposalAnswer{}, ErrStepUp
	}
	read, err := a.d.Grants.Read(ctx, p.Conversation, p.Turn)
	if err != nil {
		return wire.ProposalAnswer{}, fmt.Errorf("%w: %w", ErrNotAPerson, err)
	}
	if p.Scope == wire.ScopeServer && p.SourceRevision != "" {
		moved, err := a.moved(ctx, read, p)
		if err != nil {
			a.d.Grants.Revoke(read.ID())
			return wire.ProposalAnswer{}, &Unconfirmed{OpID: ap.Key, Cause: err}
		}
		if moved {
			return a.stale(ctx, d, p, read)
		}
	}
	w, err := a.move(ctx, d.call("approve"), p.ID, assistantdomain.ProposalMove{
		From: wire.ProposalProposed, To: wire.ProposalApproved})
	if err != nil {
		a.d.Grants.Revoke(read.ID())
		return wire.ProposalAnswer{}, err
	}
	if w.Collapsed {
		// THIS REQUEST ALREADY APPROVED IT: an earlier attempt under the
		// same key recorded the approval and this node has not applied it.
		// What that attempt went on to do is its own; nothing is sent twice.
		a.d.Grants.Revoke(read.ID())
		return a.answerNow(ctx, d, p.ID)
	}
	if p.Scope == wire.ScopePage {
		a.d.Grants.Revoke(read.ID())
		return a.sendToPage(ctx, d, p)
	}
	return a.execute(ctx, d, p, wire.ProposalApproved, read)
}

// repeat answers an approval of a proposal that is no longer waiting: the
// state it is in now when this request is a retry of the approval that
// decided it (its operation is already recorded), and [Decided] otherwise.
func (a *Approver) repeat(ctx context.Context, d decider, p assistantdomain.Proposal,
	state wire.ProposalState) (wire.ProposalAnswer, error) {

	w, err := a.d.Store.Move(ctx, d.call("approve"), p.ID, assistantdomain.ProposalMove{
		From: wire.ProposalProposed, To: wire.ProposalApproved})
	if err == nil && w.Collapsed {
		return answer(p, state, wire.TurnAccepted{TurnID: p.Turn}), nil
	}
	return wire.ProposalAnswer{}, &Decided{State: state}
}

// answerNow re-reads a proposal and answers its state now.
func (a *Approver) answerNow(ctx context.Context, d decider, id uuid.UUID) (wire.ProposalAnswer, error) {
	p, err := a.load(ctx, d.person.ID, id)
	if err != nil {
		return wire.ProposalAnswer{}, err
	}
	state := proposal.StateNow(p, a.d.Now())
	if state == wire.ProposalProposed {
		// Recorded and not yet applied here: the record's own state.
		state = wire.ProposalApproved
	}
	return answer(p, state, wire.TurnAccepted{TurnID: p.Turn}), nil
}

// answer is a decision's answer.
func answer(p assistantdomain.Proposal, state wire.ProposalState, at wire.TurnAccepted) wire.ProposalAnswer {
	turn := at.TurnID
	if turn == uuid.Nil {
		turn = p.Turn
	}
	return wire.ProposalAnswer{Proposal: wire.ProposalRef{ID: p.ID, State: state},
		TurnID: turn, StreamURL: at.StreamURL, AfterSeq: at.AfterSeq}
}

// expire records a proposal that nobody decided before its expiry. The
// proposal reads as expired whether or not the record lands, so a failure is
// logged rather than answered.
func (a *Approver) expire(ctx context.Context, d decider, p assistantdomain.Proposal) {
	if _, err := a.move(ctx, d.call("expire"), p.ID, assistantdomain.ProposalMove{
		From: wire.ProposalProposed, To: wire.ProposalExpired}); err != nil &&
		!errors.Is(err, ErrAlreadyDecided) {
		log.WarnContext(ctx, "assistant_proposal_expiry_not_recorded",
			"proposal", p.ID, "error", err)
	}
}

// moved reports whether a proposal's object has moved since its preview, read
// as the person through read. An object gone since the preview has moved.
func (a *Approver) moved(ctx context.Context, read exec.Grant, p assistantdomain.Proposal) (bool, error) {
	now, err := a.d.Revisions(ctx, a.reader(read), p.SourceRevision)
	switch {
	case errors.Is(err, proposal.ErrGone):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("approval: re-read %s: %w", p.SourceRevision, err)
	}
	return now != p.SourceRevision, nil
}

// reader sends reads through one grant.
func (a *Approver) reader(g exec.Grant) proposal.Read {
	return func(ctx context.Context, r exec.Request) (exec.Response, error) {
		return a.d.Dispatcher.Do(ctx, g, r)
	}
}

// stale records a proposal whose object moved and hands the turn the news, so
// the model may propose again against what the object is now.
func (a *Approver) stale(ctx context.Context, d decider, p assistantdomain.Proposal,
	read exec.Grant) (wire.ProposalAnswer, error) {

	if _, err := a.move(ctx, d.call("stale"), p.ID, assistantdomain.ProposalMove{
		From: wire.ProposalProposed, To: wire.ProposalStale,
		RefusalCode: CodeStale}); err != nil {
		a.d.Grants.Revoke(read.ID())
		return wire.ProposalAnswer{}, err
	}
	failure := &wire.Failure{Refusal: wire.Refusal{Code: CodeStale,
		Message: "The record changed after this was proposed, so nothing was sent."}}
	a.resume(ctx, Resumption{Owner: d.person.ID, Conversation: p.Conversation, Turn: p.Turn,
		Proposal: p.ID, Read: read, State: wire.ProposalStale, Failure: failure,
		Card: proposal.CardAt(p, wire.ProposalStale, nil, failure)})
	return wire.ProposalAnswer{}, ErrStale
}

// CodeStale is the refusal code a stale proposal carries, the API's own.
const CodeStale = "proposal_stale"

// resume hands a decision to its parked turn. The decision is recorded either
// way, so a turn that cannot be resumed is logged: the person sees the card's
// state, and the turn ends with its own expiry.
func (a *Approver) resume(ctx context.Context, r Resumption) wire.TurnAccepted {
	at, err := a.d.Turns.Resume(ctx, r)
	if err != nil {
		a.d.Grants.Revoke(r.Read.ID())
		log.WarnContext(ctx, "assistant_turn_not_resumed", "proposal", r.Proposal,
			"turn", r.Turn, "state", string(r.State), "error", err)
		return wire.TurnAccepted{TurnID: r.Turn}
	}
	return at
}

// emit appends frames to a turn that stays parked, logging a failure: the
// decision is recorded, and the card reads its state on the next load.
func (a *Approver) emit(ctx context.Context, owner uuid.UUID, p assistantdomain.Proposal,
	e Emission) wire.TurnAccepted {

	at, err := a.d.Turns.Emit(ctx, owner, p.Conversation, p.Turn, e)
	if err != nil {
		log.WarnContext(ctx, "assistant_frames_not_emitted", "proposal", p.ID,
			"turn", p.Turn, "error", err)
		return wire.TurnAccepted{TurnID: p.Turn}
	}
	return at
}

// sendToPage sends an approved page action to the page, which claims and
// reports it ([Approver.Report]). The turn stays parked until it does.
func (a *Approver) sendToPage(ctx context.Context, d decider, p assistantdomain.Proposal) (wire.ProposalAnswer, error) {
	id := p.ID
	card := proposal.CardAt(p, wire.ProposalApproved, nil, nil)
	at := a.emit(ctx, d.person.ID, p, Emission{Card: &card, Action: &wire.ActionEvent{
		ActionID: p.ID, Name: strings.TrimPrefix(p.Path, proposal.PageActionPath),
		ProposalID: &id, SourceRevision: p.SourceRevision, Payload: p.Body,
		Confirm: wire.ConfirmApprove,
	}})
	return answer(p, wire.ProposalApproved, at), nil
}

// retry sends a write whose outcome nobody could confirm again, under the
// SAME operation key, so the ledger answers it with the first attempt where
// that one landed. Asked of the person exactly as the approval was: the card
// they retry from is the one whose digest they echo.
func (a *Approver) retry(ctx context.Context, d decider, p assistantdomain.Proposal,
	digest string) (wire.ProposalAnswer, error) {

	if p.Scope != wire.ScopeServer {
		return wire.ProposalAnswer{}, &Decided{State: wire.ProposalUnknown}
	}
	if err := proposal.Verify(p, digest); err != nil {
		return wire.ProposalAnswer{}, err
	}
	if (p.StepUp || a.d.StepUp(p.Act)) && !d.person.Proved(iam.RecencyStepUp, a.d.Now()) {
		return wire.ProposalAnswer{}, ErrStepUp
	}
	if p.State != wire.ProposalUnknown {
		// UNKNOWN IS DECIDED ON READ; record it before it is left, so the
		// log says the outcome was in doubt and the Retry is its own move.
		if _, err := a.move(ctx, d.call("unknown"), p.ID, assistantdomain.ProposalMove{
			From: p.State, To: wire.ProposalUnknown}); err != nil {
			return wire.ProposalAnswer{}, err
		}
	}
	read, err := a.d.Grants.Read(ctx, p.Conversation, p.Turn)
	if err != nil {
		return wire.ProposalAnswer{}, fmt.Errorf("%w: %w", ErrNotAPerson, err)
	}
	return a.execute(ctx, d, p, wire.ProposalUnknown, read)
}
