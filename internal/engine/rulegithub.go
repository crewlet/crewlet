package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
)

// The company's GitHub as a delegated-decisions rule offers to name it
// (ADR-0125), read through the agents' own apps of the wiring now in force and
// held to the owners the company names, as a rule's required checks are.

// errGitHubOff is a read on a node whose company runs no GitHub now.
var errGitHubOff = fmt.Errorf("engine: %w: the company runs no GitHub now", integration.ErrNotConfigured)

// GitHubRepositories is every repository an agent's app covers under one of
// the company's owners.
func (e *Engine) GitHubRepositories(ctx context.Context) ([]github.InventoryRepo, error) {
	lookup, owners := e.githubPulls.current()
	if lookup == nil {
		return nil, errGitHubOff
	}
	repos, err := lookup.Repositories(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(repos, func(r github.InventoryRepo) bool {
		owner, _, _ := strings.Cut(r.FullName, "/")
		return !owned(owners, owner)
	}), nil
}

// GitHubBranches is owner/repo's branches, and whether GitHub holds more than
// were read.
func (e *Engine) GitHubBranches(ctx context.Context, owner, repo string) ([]string, bool, error) {
	lookup, err := e.ruleLookup(owner)
	if err != nil {
		return nil, false, err
	}
	return lookup.Branches(ctx, owner, repo)
}

// GitHubCheckNames is the check names on branch's newest commit in owner/repo.
func (e *Engine) GitHubCheckNames(ctx context.Context, owner, repo, branch string) ([]string, error) {
	lookup, err := e.ruleLookup(owner)
	if err != nil {
		return nil, err
	}
	return lookup.CheckNames(ctx, owner, repo, branch)
}

// ruleLookup is the lookup to read a repository under owner through.
func (e *Engine) ruleLookup(owner string) (*github.SeatLookup, error) {
	lookup, owners := e.githubPulls.current()
	switch {
	case lookup == nil:
		return nil, errGitHubOff
	case !owned(owners, owner):
		return nil, fmt.Errorf("%w: %s", github.ErrOutsideCompany, owner)
	}
	return lookup, nil
}
