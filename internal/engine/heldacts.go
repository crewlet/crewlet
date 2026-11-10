package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// heldActs is the decision a seat's held call carries out, read off the
// tracker this node reads: [builtin.ActedDecisions].
type heldActs struct{ engine *Engine }

// DecisionActedOn is the newest question asker put on item that a person
// answered, nil on a node with no tracker. A stale read: the call is held
// either way, and what it names is for the approver to read.
func (h heldActs) DecisionActedOn(ctx context.Context, item, asker string) (*sandbox.HeldAct, error) {
	tracker := h.engine.Tracker()
	if tracker == nil {
		return nil, nil
	}
	acted, err := tracker.DecisionActedOn(ctx, item, asker, statelog.ReadStale)
	if err != nil || acted == nil {
		return nil, err
	}
	return &sandbox.HeldAct{Ask: acted.Ask, Question: acted.Question, Choice: acted.Choice,
		AnsweredBy: acted.AnsweredBy, AnsweredAt: acted.AnsweredAt}, nil
}
