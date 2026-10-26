package approval

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// Decline records the person's decline of one of their proposals and resumes
// the turn with it, so the model is told nothing was done. The echoed digest
// must be the card's, as for an approval: a decline of something the person
// was not shown is refused rather than recorded.
func (a *Approver) Decline(ctx context.Context, ap Approval) (wire.ProposalAnswer, error) {
	d, err := a.deciderOf(ctx, ap.Key, ap.Page)
	if err != nil {
		return wire.ProposalAnswer{}, err
	}
	p, err := a.load(ctx, d.person.ID, ap.Proposal)
	if err != nil {
		return wire.ProposalAnswer{}, err
	}
	switch state := proposal.StateNow(p, a.d.Now()); {
	case state == wire.ProposalExpired && p.State == wire.ProposalProposed:
		a.expire(ctx, d, p)
		return wire.ProposalAnswer{}, ErrExpired
	case state != wire.ProposalProposed:
		if err = a.repeated(ctx, d.call("decline"), p, wire.ProposalDeclined, state); err != nil {
			return wire.ProposalAnswer{}, err
		}
		return answer(p, state, wire.TurnAccepted{TurnID: p.Turn}), nil
	}
	if err = proposal.Verify(p, ap.Digest); err != nil {
		return wire.ProposalAnswer{}, err
	}
	w, err := a.move(ctx, d.call("decline"), p.ID, assistantdomain.ProposalMove{
		From: wire.ProposalProposed, To: wire.ProposalDeclined})
	if err != nil {
		return wire.ProposalAnswer{}, err
	}
	if w.Collapsed {
		return answer(p, wire.ProposalDeclined, wire.TurnAccepted{TurnID: p.Turn}), nil
	}
	read, err := a.d.Grants.Read(ctx, p.Conversation, p.Turn)
	if err != nil {
		// DECLINED ALL THE SAME: a decline sends nothing, so a session that
		// can no longer carry the turn changes only that the turn ends with
		// its own expiry rather than resuming.
		log.InfoContext(ctx, "assistant_decline_not_resumed", "proposal", p.ID, "error", err)
		return answer(p, wire.ProposalDeclined, wire.TurnAccepted{TurnID: p.Turn}), nil
	}
	at := a.resume(ctx, Resumption{Owner: d.person.ID, Page: d.page, Conversation: p.Conversation,
		Turn: p.Turn, Proposal: p.ID, Read: read, State: wire.ProposalDeclined,
		Card: proposal.CardAt(p, wire.ProposalDeclined, nil, nil)})
	return answer(p, wire.ProposalDeclined, at), nil
}

// Stop is one Stop of a turn: whose it is, which turn, and the stopping
// request's operation.
type Stop struct {
	Owner, Conversation, Turn uuid.UUID
	By                        iam.Actor
	Key                       string
}

// DeclineTurn declines every proposal a stopped turn is still waiting on: a
// Stop on a parked turn is the person saying "none of this". It resumes
// nothing, because the turn is ending.
//
// THE HOOK the assistant's service calls from its Stop, with the stopping
// request's own owner and operation.
func (a *Approver) DeclineTurn(ctx context.Context, s Stop) error {
	proposals, err := a.d.Store.TurnProposals(ctx, s.Owner, s.Conversation, s.Turn)
	if err != nil {
		return err
	}
	var errs []error
	now := a.d.Now()
	for _, p := range proposals {
		if proposal.StateNow(p, now) != wire.ProposalProposed {
			continue
		}
		c := assistantdomain.Call{Owner: s.Owner, By: s.By,
			OpID: a.d.StepOp(s.Key, "stop", p.ID.String())}
		if _, err := a.move(ctx, c, p.ID, assistantdomain.ProposalMove{
			From: wire.ProposalProposed, To: wire.ProposalDeclined}); err != nil &&
			!errors.Is(err, ErrAlreadyDecided) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
