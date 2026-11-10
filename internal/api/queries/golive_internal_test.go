package queries

import (
	"context"
	"errors"
	"testing"
)

// THE GO-LIVE REVIEW IS EVERY AGENT'S ACCESS AS THE ENGINE READS IT, always a
// list, and a read that fails is the answer's failure, never an empty review.
func TestTheGoLiveReviewIsEveryAgentsAccess(t *testing.T) {
	t.Parallel()
	s := Sources{AgentAccess: func(context.Context) ([]AgentAccessRow, error) { return nil, nil }}
	got, err := s.goLiveReview(t.Context(), Params{})
	if err != nil || got.(GoLiveReviewAnswer).Agents == nil {
		t.Errorf("an engine with no agents answered %+v, %v, want an empty list", got, err)
	}
	s.AgentAccess = func(context.Context) ([]AgentAccessRow, error) {
		return nil, errors.New("the access ledger did not answer")
	}
	if _, err := s.goLiveReview(t.Context(), Params{}); err == nil {
		t.Error("a failed read answered a review")
	}
}
