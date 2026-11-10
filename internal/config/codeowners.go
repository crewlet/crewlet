package config

import (
	"regexp"
	"slices"
	"strings"
	"sync"
)

// CodeOwners is the owners a code host's configuration names: each of owners
// and the owner of each `owner/repository` path, expanded, lower-cased and
// sorted. A segment no code host allows as an owner is dropped.
func CodeOwners(owners, paths []string, expand func(string) string) []string {
	var out []string
	keep := func(owner string) {
		owner = strings.ToLower(owner)
		if ownerName().MatchString(owner) && !slices.Contains(out, owner) {
			out = append(out, owner)
		}
	}
	for _, owner := range owners {
		keep(expand(owner))
	}
	for _, path := range paths {
		if owner, _, ok := strings.Cut(expand(path), "/"); ok {
			keep(owner)
		}
	}
	slices.Sort(out)
	return out
}

// GitHubOwners is the owners the company's GitHub configuration names: its
// organization and the owner of every repository a seat works in. The one
// reading of which code on GitHub is the company's.
func (c *Company) GitHubOwners(expand func(string) string) []string {
	gh := c.Integrations.GitHub
	if gh == nil {
		return nil
	}
	var orgs, repos []string
	if p := gh.Provisioning; p != nil {
		orgs, repos = []string{p.Org}, p.Repos
	}
	for role := range c.EachRole() {
		if rg := role.Integrations.GitHub; rg != nil {
			repos = append(repos, rg.Repos...)
		}
	}
	return CodeOwners(orgs, repos, expand)
}

// ownerName is a GitHub organization's or a GitLab group's path: letters,
// digits, `.`, `_` and `-`, starting with a letter or a digit. Compiled at
// first use, since Gitlet's push hook is this binary and pays every init.
var ownerName = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)
})
