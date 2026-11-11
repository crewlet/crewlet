package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// heldActs is the decision a seat's held call carries out, read off the
// tracker this node reads: [builtin.ActedDecisions].
type heldActs struct{ engine *Engine }

// DecisionAnswered is the newest decision a person gave asker among answers,
// nil on a node with no tracker. A stale read: the call is held either way,
// and what it names is for the approver to read.
func (h heldActs) DecisionAnswered(ctx context.Context, answers []string, asker string) (*sandbox.HeldAct, error) {
	tracker := h.engine.Tracker()
	if tracker == nil {
		return nil, nil
	}
	acted, err := tracker.DecisionAnswered(ctx, answers, asker, statelog.ReadStale)
	if err != nil || acted == nil {
		return nil, err
	}
	return &sandbox.HeldAct{Item: acted.Task, Key: acted.Key, Ask: acted.Ask, Question: acted.Question,
		Choice: acted.Choice, ChoiceID: acted.ChoiceID, Act: acted.Act,
		AnsweredBy: acted.AnsweredBy, AnsweredAt: acted.AnsweredAt}, nil
}
