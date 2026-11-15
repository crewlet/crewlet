package github

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// What a delegated-decisions rule offers to name from the company's GitHub
// (ADR-0125): repositories, branches and check names, read through the agents' apps.

// ErrOutsideCompany reports a repository under an owner the company's GitHub
// does not name, where nothing is read for the company.
var ErrOutsideCompany = errors.New("github: the repository is not under an owner the company's GitHub names")

// branchPages bounds a branch listing at three pages of [factPageSize]: a rule
// names the few branches a company releases from, and one past the listing is
// typed rather than costing a repository of thousands a request per hundred.
const branchPages = 3

// Repositories is every repository an installed agent's app covers, each once
// (GitHub names one case-insensitively), sorted by name. A seat that cannot
// list is skipped; none listing is [ErrNoSeatCanRead].
func (l *SeatLookup) Repositories(ctx context.Context) ([]InventoryRepo, error) {
	byName := map[string]InventoryRepo{}
	var last error
	listed := false
	for _, seat := range l.Opts.Seats {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !Issuable(seat) {
			continue
		}
		token, err := l.token(ctx, seat)
		if err != nil {
			last = err
			continue
		}
		client, err := NewClient(ClientOptions{APIBase: l.Opts.APIBase, WebBase: l.Opts.WebBase, Token: token})
		if err != nil {
			last = err
			continue
		}
		repos, err := walkRepositories(func(path string, out any) error { return client.get(ctx, path, nil, out) })
		if err != nil {
			last = err
			continue
		}
		listed = true
		for _, repo := range repos {
			if key := strings.ToLower(repo.FullName); byName[key].FullName == "" {
				byName[key] = repo
			}
		}
	}
	switch {
	case !listed && last != nil:
		return nil, fmt.Errorf("%w: %w", ErrNoSeatCanRead, last)
	case !listed:
		return nil, ErrNoSeatCanRead
	}
	return slices.SortedFunc(maps.Values(byName), func(a, b InventoryRepo) int {
		return cmp.Compare(strings.ToLower(a.FullName), strings.ToLower(b.FullName))
	}), nil
}

// Branches is repo's branches in GitHub's order, read through the first
// agent's app that covers it, and whether GitHub holds more than
// [branchPages] pages of them.
func (l *SeatLookup) Branches(ctx context.Context, owner, repo string) ([]string, bool, error) {
	var names []string
	var more bool
	err := l.read(ctx, func(c *Client) error {
		names, more = nil, false
		path := repoPath(owner, repo) + "/branches"
		for page := 1; page <= branchPages; page++ {
			var rows []struct {
				Name string `json:"name"`
			}
			query := url.Values{"per_page": {strconv.Itoa(factPageSize)}, "page": {strconv.Itoa(page)}}
			if err := c.get(ctx, path, query, &rows); err != nil {
				return err
			}
			for _, row := range rows {
				if row.Name != "" {
					names = append(names, row.Name)
				}
			}
			if len(rows) < factPageSize {
				return nil
			}
		}
		// ONE MORE ROW tells a listing of exactly that many from a longer one.
		var past []struct{}
		query := url.Values{"per_page": {"1"}, "page": {strconv.Itoa(branchPages*factPageSize + 1)}}
		if err := c.get(ctx, path, query, &past); err != nil {
			return err
		}
		more = len(past) > 0
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return names, more, nil
}

// CheckNames is the check run names on branch's newest commit, once and sorted,
// leaving out runs no app or an agent's own app reported: neither passes one.
func (l *SeatLookup) CheckNames(ctx context.Context, owner, repo, branch string) ([]string, error) {
	var agents Reporters
	for _, seat := range l.Opts.Seats {
		if seat.AppID != 0 {
			agents.AppIDs = append(agents.AppIDs, seat.AppID)
		}
	}
	var names []string
	err := l.read(ctx, func(c *Client) error {
		var page checkRunsWire
		query := url.Values{"per_page": {strconv.Itoa(factPageSize)}, "filter": {"latest"}}
		path := repoPath(owner, repo) + "/commits/" + url.PathEscape(branch) + "/check-runs"
		if err := c.get(ctx, path, query, &page); err != nil {
			return err
		}
		names = nil
		for _, run := range page.CheckRuns {
			if id := run.appID(); id != 0 && !agents.app(id) && run.Name != "" && !slices.Contains(names, run.Name) {
				names = append(names, run.Name)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	return names, nil
}
