package queries

import (
	"context"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// codeRepoReply is code_repo's answer: the read's, and the address a person's git clones it from.
type codeRepoReply struct {
	gitlet.ReadAnswer
	CloneURL string `json:"clone_url,omitempty"`
}

// codeRepo answers code_repo with its clone address on this deployment's own address
// (`api.external_url`), none where this node names none. Built here, not by the data node
// that read the repository, which does not hold the asking node's address.
func (s Sources) codeRepo() Answer {
	return func(ctx context.Context, p Params) (any, error) {
		answer, err := s.readCode(ctx, gitlet.ReadRepo, "code_repo", p)
		if err != nil {
			return nil, err
		}
		reply := codeRepoReply{ReadAnswer: answer}
		if reply.Repo != nil && s.DashboardBase != nil {
			if base := s.DashboardBase(); base != "" {
				reply.CloneURL = base + auth.GitRepoPath(reply.Repo.Name)
			}
		}
		return reply, nil
	}
}
