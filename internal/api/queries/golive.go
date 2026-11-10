package queries

import (
	"context"

	"github.com/crewlet/crewlet/internal/capability"
)

// GoLiveReviewAnswer is `go_live_review`: what each agent may do and where it
// runs, as its next turn is told, for the go-live check's review.
type GoLiveReviewAnswer struct {
	Agents []AgentAccessRow `json:"agents"`
}

// AgentAccessRow is one agent's access, by its handle and name.
type AgentAccessRow struct {
	Seat   string              `json:"seat"`
	Name   string              `json:"name"`
	Access capability.Snapshot `json:"access"`
}

// AgentAccess reads every agent's [AgentAccessRow], the engine's.
type AgentAccess func(ctx context.Context) ([]AgentAccessRow, error)

func (s Sources) goLiveReview(ctx context.Context, _ Params) (any, error) {
	rows, err := s.AgentAccess(ctx)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []AgentAccessRow{}
	}
	return GoLiveReviewAnswer{Agents: rows}, nil
}
