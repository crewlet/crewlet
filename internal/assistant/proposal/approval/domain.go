package approval

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// Domain is [Store] over the assistant's own domain: its owner-keyed reads and
// its arbitrated proposal move.
func Domain(w *assistantdomain.Writer, r *assistantdomain.Reader) (Store, error) {
	if w == nil || r == nil {
		return nil, errors.New("approval: the assistant domain's writer and reader are " +
			"both required")
	}
	return domainStore{w: w, r: r}, nil
}

type domainStore struct {
	w *assistantdomain.Writer
	r *assistantdomain.Reader
}

func (s domainStore) Proposal(ctx context.Context, owner, id uuid.UUID) (assistantdomain.Proposal, error) {
	return s.r.Proposal(ctx, owner, id)
}

func (s domainStore) TurnProposals(ctx context.Context, owner, conversation,
	turn uuid.UUID) ([]assistantdomain.Proposal, error) {

	t, err := s.r.Transcript(ctx, owner, conversation)
	if err != nil {
		return nil, err
	}
	var out []assistantdomain.Proposal
	for _, p := range t.Proposals {
		if p.Turn == turn {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s domainStore) Move(ctx context.Context, c assistantdomain.Call, id uuid.UUID,
	m assistantdomain.ProposalMove) (Written, error) {

	res, err := s.w.MoveProposal(ctx, c, id, m)
	if err != nil {
		return Written{}, err
	}
	return Written{Outcome: string(res.Outcome), Collapsed: res.Collapsed}, nil
}
