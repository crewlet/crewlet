package engine

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/github"
)

// The company's GitHub pull requests as ask_to_merge and ask_to_deploy read
// and merge them (ADR-0125): every read and the merge as the requesting seat's
// own app, never another seat's, so what an approval carries out is what that
// seat's credential and its branch's rules allow, and the facts a delegate's
// allow is held to are the ones that credential sees.

// releasePulls is [builtin.PullRequests] over the current GitHub wiring.
type releasePulls struct{ engine *Engine }

var _ builtin.PullRequests = releasePulls{}

// errNoGitHubWired is a release request's read on a node whose company runs no
// GitHub now.
var errNoGitHubWired = errors.New("engine: this company runs no GitHub now, so no pull request is read")

// owned reports whether owner is one the company's GitHub names.
func owned(owners []string, owner string) bool {
	return slices.ContainsFunc(owners, func(o string) bool { return strings.EqualFold(o, owner) })
}

// Ref is the pull request address names under one of the company's owners.
func (p releasePulls) Ref(address string) (builtin.PullRef, bool) {
	lookup, owners := p.engine.githubPulls.current()
	if lookup == nil {
		return builtin.PullRef{}, false
	}
	ref, ok := github.ParsePullURL(lookup.Opts.WebBase, address)
	if !ok || !owned(owners, ref.Owner) {
		return builtin.PullRef{}, false
	}
	return builtin.PullRef{Owner: strings.ToLower(ref.Owner), Repo: strings.ToLower(ref.Repo),
		Number: ref.Number}, true
}

// Repo is the repository address names under one of the company's owners: its
// own page on the company's GitHub, with or without a trailing `.git`.
func (p releasePulls) Repo(address string) (builtin.PullRef, bool) {
	lookup, owners := p.engine.githubPulls.current()
	if lookup == nil {
		return builtin.PullRef{}, false
	}
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(lookup.Opts.WebBase), "/"))
	if err != nil || base.Host == "" {
		return builtin.PullRef{}, false
	}
	link, err := url.Parse(strings.TrimSpace(address))
	if err != nil || !strings.EqualFold(link.Scheme, base.Scheme) || !strings.EqualFold(link.Host, base.Host) ||
		link.RawQuery != "" || link.Fragment != "" {
		return builtin.PullRef{}, false
	}
	rest, ok := strings.CutPrefix(link.Path, base.Path+"/")
	parts := strings.Split(strings.TrimSuffix(strings.TrimRight(rest, "/"), ".git"), "/")
	if !ok || len(parts) != 2 || parts[0] == "" || parts[1] == "" || !owned(owners, parts[0]) {
		return builtin.PullRef{}, false
	}
	return builtin.PullRef{Owner: strings.ToLower(parts[0]), Repo: strings.ToLower(parts[1])}, true
}

// client is a client acting as seat's own app.
func (p releasePulls) client(ctx context.Context, seat string) (*github.Client, *github.SeatLookup, error) {
	lookup, _ := p.engine.githubPulls.current()
	if lookup == nil {
		return nil, nil, errNoGitHubWired
	}
	c, err := lookup.ClientFor(ctx, seat)
	return c, lookup, err
}

// Facts are a pull request's facts as seat reads them, with every login a
// seat's app wrote named as that seat.
func (p releasePulls) Facts(ctx context.Context, seat string, ref builtin.PullRef) (builtin.PullFacts, error) {
	c, lookup, err := p.client(ctx, seat)
	if err != nil {
		return builtin.PullFacts{}, err
	}
	f, err := c.PullFacts(ctx, ref.Owner, ref.Repo, ref.Number, agentReporters(lookup))
	if err != nil {
		return builtin.PullFacts{}, err
	}
	paths, complete, err := c.PullFiles(ctx, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return builtin.PullFacts{}, err
	}
	who := func(login string) string {
		if handle, ok := lookup.Opts.SeatOfLogin(login); ok {
			return handle
		}
		return login
	}
	out := builtin.PullFacts{Title: f.Title, Author: who(f.Author), Open: f.State == "open", Merged: f.Merged,
		Draft: f.Draft, Head: strings.ToLower(f.HeadSHA), Base: f.BaseRef,
		MergeCommit: strings.ToLower(f.MergeCommitSHA), Mergeable: f.Mergeable, MergeableState: f.MergeableState,
		Labels: f.Labels, Required: checkFacts(f.Checks), RequiredRead: true, Paths: paths,
		PathsComplete: complete}
	for _, login := range f.Approvals {
		out.Approvers = append(out.Approvers, who(login))
	}
	return out, nil
}

// agentReporters are the company's own agents' apps, whose report of a check
// counts for nothing in a release request: an agent whose app may write
// checks would otherwise report its own change as passing.
func agentReporters(lookup *github.SeatLookup) github.Reporters {
	var out github.Reporters
	for _, seat := range lookup.Opts.Seats {
		if seat.AppID != 0 {
			out.AppIDs = append(out.AppIDs, seat.AppID)
		}
		if login := github.BotLogin(seat.Slug); login != "" {
			out.Logins = append(out.Logins, login)
		}
	}
	return out
}

// checkFacts are a commit's required checks as the release requests read them.
func checkFacts(results []github.CheckResult) []builtin.CheckFact {
	out := make([]builtin.CheckFact, 0, len(results))
	for _, r := range results {
		out = append(out, builtin.CheckFact{Name: r.Context, Result: string(r.State)})
	}
	return out
}

// Contains reports whether commit holds ancestor, both whole commit ids.
func (p releasePulls) Contains(ctx context.Context, seat string, repo builtin.PullRef, commit,
	ancestor string) (bool, error) {

	c, _, err := p.client(ctx, seat)
	if err != nil {
		return false, err
	}
	return c.Contains(ctx, repo.Owner, repo.Repo, commit, ancestor)
}

// OnBranch reports whether branch's tip holds commit.
func (p releasePulls) OnBranch(ctx context.Context, seat string, repo builtin.PullRef, branch,
	commit string) (bool, error) {

	c, _, err := p.client(ctx, seat)
	if err != nil {
		return false, err
	}
	tip, err := c.BranchTip(ctx, repo.Owner, repo.Repo, branch)
	if err != nil {
		return false, err
	}
	return c.Contains(ctx, repo.Owner, repo.Repo, tip, commit)
}

// Checks are the results on commit of every check branch requires.
func (p releasePulls) Checks(ctx context.Context, seat string, repo builtin.PullRef, branch,
	commit string) ([]builtin.CheckFact, bool, error) {

	c, lookup, err := p.client(ctx, seat)
	if err != nil {
		return nil, false, err
	}
	required, err := c.RequiredChecks(ctx, repo.Owner, repo.Repo, branch)
	if err != nil {
		return nil, false, err
	}
	results, err := c.CommitChecks(ctx, repo.Owner, repo.Repo, commit, required, agentReporters(lookup))
	if err != nil {
		return nil, false, err
	}
	return checkFacts(results), true, nil
}

// Merge merges the pull request at exactly head as seat's app, GitHub's
// refusals named as the tool words them.
func (p releasePulls) Merge(ctx context.Context, seat string, ref builtin.PullRef, head, method string) (string,
	error) {

	c, _, err := p.client(ctx, seat)
	if err != nil {
		return "", err
	}
	res, err := c.MergePull(ctx, ref.Owner, ref.Repo, ref.Number, head, github.MergeMethod(method))
	switch {
	case errors.Is(err, github.ErrHeadMoved):
		return "", errors.Join(builtin.ErrPullMoved, err)
	case errors.Is(err, github.ErrNotMergeable), errors.Is(err, github.ErrMergeInvalid):
		return "", errors.Join(builtin.ErrPullNotMergeable, err)
	case errors.Is(err, github.ErrMergeForbidden):
		return "", errors.Join(builtin.ErrPullForbidden, err)
	case err != nil:
		return "", err
	}
	return strings.ToLower(res.SHA), nil
}
