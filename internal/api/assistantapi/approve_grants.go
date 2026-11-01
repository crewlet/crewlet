package assistantapi

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// Grants is the approval path's grants over the API's own delegations: every
// grant derived from the deciding request the guard resolved, and never from
// anything else, so a decision acts on the click that made it.
func Grants(d *auth.Delegations) (approval.Grants, error) {
	if d == nil {
		return nil, errors.New("assistantapi: the assistant's delegations are required: " +
			"an approved write is sent under a grant derived from the approval")
	}
	return delegated{d: d}, nil
}

type delegated struct{ d *auth.Delegations }

func (g delegated) Read(ctx context.Context, conversation, turn uuid.UUID) (exec.Grant, error) {
	return g.d.Derive(ctx, auth.AssistRequest{Conversation: conversation, Turn: turn,
		Scope: iam.AssistScopeRead})
}

func (g delegated) Exec(ctx context.Context, x approval.Exec) (exec.Grant, error) {
	request := x.Request
	return g.d.Derive(ctx, auth.AssistRequest{Conversation: x.Conversation, Turn: x.Turn,
		Scope: iam.AssistScopeExec, Proposal: x.Proposal, Action: authz.Action(x.Act),
		Request: &request})
}

func (g delegated) Revoke(grant uuid.UUID) { g.d.Revoke(grant) }
