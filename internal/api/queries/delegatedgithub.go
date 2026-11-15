package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/integration"
)

// What a delegated-decisions rule offers to name from the company's GitHub
// (ADR-0125): `GET /delegated-decisions/repositories`, `/branches?repo=` and
// `/checks?repo=&branch=`, read through the agents' own apps for the rule's
// required checks. What could not be read is said in `unread`, never an empty
// list that reads as "there are none", and a repository can always be typed.
//
// AN ADMINISTRATOR'S, as the company document a rule is written into is.

// DelegatedGitHub is the company's GitHub as the rule's form reads it. A
// company that runs no GitHub answers [integration.ErrNotConfigured], and a
// repository under an owner it does not name [github.ErrOutsideCompany].
type DelegatedGitHub interface {
	GitHubRepositories(ctx context.Context) ([]github.InventoryRepo, error)
	GitHubBranches(ctx context.Context, owner, repo string) ([]string, bool, error)
	GitHubCheckNames(ctx context.Context, owner, repo, branch string) ([]string, error)
}

// DelegatedRepositoriesAnswer is `delegated_repositories`: the company's
// GitHub repositories an agent's app covers.
type DelegatedRepositoriesAnswer struct {
	// Connected is false for a company that runs no GitHub.
	Connected bool                  `json:"connected"`
	Repos     []DelegatedRepository `json:"repos"`
	Unread    string                `json:"unread,omitempty"`
}

// DelegatedRepository is one repository, as owner/name.
type DelegatedRepository struct {
	Repo          string `json:"repo"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
}

// DelegatedBranchesAnswer is `delegated_branches`: one repository's branches.
type DelegatedBranchesAnswer struct {
	Repo     string   `json:"repo"`
	Branches []string `json:"branches"`
	// More is GitHub holding more branches than were read.
	More   bool   `json:"more"`
	Unread string `json:"unread,omitempty"`
}

// DelegatedChecksAnswer is `delegated_checks`: the check names on a branch's
// newest commit that a rule can require.
type DelegatedChecksAnswer struct {
	Repo   string   `json:"repo"`
	Branch string   `json:"branch"`
	Checks []string `json:"checks"`
	Unread string   `json:"unread,omitempty"`
}

// delegatedRepositories answers `delegated_repositories`.
func (s Sources) delegatedRepositories(ctx context.Context, _ Params) (any, error) {
	out := DelegatedRepositoriesAnswer{Connected: true, Repos: []DelegatedRepository{}}
	repos, err := s.DelegatedGitHub.GitHubRepositories(ctx)
	switch {
	case errors.Is(err, integration.ErrNotConfigured):
		out.Connected = false
		return out, nil
	case err != nil:
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		out.Unread = githubUnread(err, "the company's repositories")
		return out, nil
	}
	for _, r := range repos {
		out.Repos = append(out.Repos, DelegatedRepository{Repo: r.FullName, DefaultBranch: r.DefaultBranch,
			Archived: r.Archived})
	}
	return out, nil
}

// delegatedBranches answers `delegated_branches`, `repo=` naming owner/name.
func (s Sources) delegatedBranches(ctx context.Context, p Params) (any, error) {
	owner, name, err := ruleRepo(p)
	if err != nil {
		return nil, err
	}
	repo := owner + "/" + name
	out := DelegatedBranchesAnswer{Repo: repo, Branches: []string{}}
	branches, more, err := s.DelegatedGitHub.GitHubBranches(ctx, owner, name)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		out.Unread = githubUnread(err, repo+"'s branches")
		return out, nil
	}
	out.Branches, out.More = append(out.Branches, branches...), more
	return out, nil
}

// delegatedChecks answers `delegated_checks`, `repo=` naming owner/name and
// `branch=` one of its branches.
func (s Sources) delegatedChecks(ctx context.Context, p Params) (any, error) {
	owner, name, err := ruleRepo(p)
	if err != nil {
		return nil, err
	}
	branch := strings.TrimSpace(p.String("branch"))
	if !gitref.ValidBranch(branch) {
		return nil, fmt.Errorf("%w: branch is a branch name such as main, and %q isn't", ErrBadParams, branch)
	}
	repo := owner + "/" + name
	out := DelegatedChecksAnswer{Repo: repo, Branch: branch, Checks: []string{}}
	checks, err := s.DelegatedGitHub.GitHubCheckNames(ctx, owner, name, branch)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var apiErr *github.APIError
		if errors.As(err, &apiErr) && apiErr.Forbidden() {
			out.Unread = "No agent's GitHub app may read check runs on " + repo +
				": give one Checks (Read-only), or type the check's name."
		} else {
			out.Unread = githubUnread(err, "the checks on "+repo+"'s "+branch)
		}
		return out, nil
	}
	out.Checks = append(out.Checks, checks...)
	return out, nil
}

// ruleRepo is `repo=` as owner and name.
func ruleRepo(p Params) (string, string, error) {
	raw := strings.TrimSpace(p.String("repo"))
	owner, name, ok := strings.Cut(raw, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("%w: repo is a GitHub repository as owner/name, such as acme/api, and %q isn't",
			ErrBadParams, raw)
	}
	return owner, name, nil
}

// githubUnread is why what could not be read, a person's sentence naming it.
func githubUnread(err error, what string) string {
	switch {
	case errors.Is(err, integration.ErrNotConfigured):
		return "The company runs no GitHub, so " + what + " cannot be read. Type the name instead."
	case errors.Is(err, github.ErrOutsideCompany):
		return "Its owner is not one the company's GitHub names, so " + what +
			" cannot be read and a rule's checks there are never read."
	case errors.Is(err, github.ErrNoSeatCanRead):
		return "No agent's GitHub app could read " + what +
			": install an agent's app there, or type the name."
	}
	return "GitHub did not answer for " + what + " just now. Try again, or type the name."
}
