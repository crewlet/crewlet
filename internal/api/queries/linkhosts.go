package queries

import (
	"context"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
)

// LinkHostKind names the third-party app a link host belongs to. A named
// string with a closed set, so the dashboard's mark for a host is chosen from
// values this file states rather than from whatever a row happens to say.
type LinkHostKind string

// The apps a link host can belong to: the code hosts, the chat, and the
// tracker and wiki on an Atlassian site.
const (
	LinkHostGitHub     LinkHostKind = "github"
	LinkHostGitLab     LinkHostKind = "gitlab"
	LinkHostMattermost LinkHostKind = "mattermost"
	LinkHostJira       LinkHostKind = "jira"
	LinkHostConfluence LinkHostKind = "confluence"
)

// LinkHostKinds is every kind this answer sends, the closed set the
// dashboard's `LinkHostKind` declares.
var LinkHostKinds = []LinkHostKind{
	LinkHostGitHub, LinkHostGitLab, LinkHostMattermost, LinkHostJira, LinkHostConfluence,
}

// Valid reports whether k is one of the kinds this answer sends.
func (k LinkHostKind) Valid() bool { return slices.Contains(LinkHostKinds, k) }

// LinkHost is one configured app's browser address and, for a code host, the
// owners whose repositories the company works in.
type LinkHost struct {
	Kind LinkHostKind `json:"kind"`
	URL  string       `json:"url"`
	// Owners are a code host's organizations (GitHub) or top-level groups
	// (GitLab), sorted, each once. Absent on every other kind. See
	// [codeOwners].
	Owners []string `json:"owners,omitempty"`
}

// linkHosts answers where the company's own apps live in a browser: the base
// a person opens a pull request, a channel or an issue at, per configured app.
//
// WHY IT EXISTS. The dashboard draws a vendor's mark beside a link to that
// vendor and turns a bare `owner/repo#14` into a link, and both need the
// company's hosts: an Enterprise Server, a self-hosted GitLab or Mattermost
// and a Jira site live at addresses no rule can guess. The `integrations`
// answer holds them, and it is an administrator's, for the reason its
// registration gives: it is a map of what is configured and what is not.
//
// SO THIS IS THE LEAST A READER NEEDS, AND IT IS EVERYBODY'S: a kind and a
// base address per app the company configured, and a code host's OWNERS (its
// organizations or top-level groups, [codeOwners]). No repository, project or
// team (which repositories a company works in is the integration's
// business), no credential, no enabled flag and no health. Every reader of
// the company's work already reads these hosts and owners in the links and
// references its agents write.
//
// THE ADDRESS IS REDUCED TO WHERE A BROWSER GOES: scheme, host, port and a
// path prefix, with any user information, query and fragment dropped, and a
// scheme other than http or https refused outright. An operator who wrote a
// credential into an instance URL has made a mistake this answer must not
// publish to every reader.
//
// RESOLVED through the serving node's chain ([Sources.Env]), as every caller
// that builds a link from these fields resolves them, so a host written as a
// `${VAR}` is the host the engine itself links to, never the reference.
func (s Sources) linkHosts(_ context.Context, _ Params) (any, error) {
	company, _ := s.Company()
	hosts := []LinkHost{}
	if company == nil {
		return map[string]any{"hosts": hosts}, nil
	}
	lookup := s.Env
	if lookup == nil {
		lookup = os.LookupEnv
	}
	expand := func(value string) string {
		out, _ := envref.Expand(value, lookup)
		return strings.TrimSpace(out)
	}
	add := func(kind LinkHostKind, raw string, owners []string) {
		if base := browserBase(raw); base != "" {
			hosts = append(hosts, LinkHost{Kind: kind, URL: base, Owners: owners})
		}
	}
	in := company.Integrations
	if in.GitHub != nil {
		_, web := in.GitHub.Bases(lookup)
		var named []string
		if p := in.GitHub.Provisioning; p != nil {
			named = append(append(named, p.Org), p.Repos...)
		}
		for role := range company.EachRole() {
			if gh := role.Integrations.GitHub; gh != nil {
				named = append(named, gh.Repos...)
			}
		}
		add(LinkHostGitHub, web, codeOwners(named, expand))
	}
	if in.GitLab != nil {
		var named []string
		if p := in.GitLab.Provisioning; p != nil {
			named = append(append(named, p.Group), p.Projects...)
		}
		add(LinkHostGitLab, expand(in.GitLab.URL), codeOwners(named, expand))
	}
	if in.Mattermost != nil {
		add(LinkHostMattermost, expand(in.Mattermost.URL), nil)
	}
	// THE SHAREABLE BASE, never the REST one: a cloud id's API gateway is
	// not a place a browser goes, and a cloud id with no site URL has no
	// shareable base at all, so it adds no host.
	if in.Jira != nil {
		add(LinkHostJira, (&config.Jira{URL: expand(in.Jira.URL), SiteURL: expand(in.Jira.SiteURL)}).ShareableBaseURL(), nil)
	}
	if in.Confluence != nil {
		add(LinkHostConfluence, (&config.Confluence{URL: expand(in.Confluence.URL), SiteURL: expand(in.Confluence.SiteURL)}).ShareableBaseURL(), nil)
	}
	return map[string]any{"hosts": hosts}, nil
}

// browserBase is raw as the address a browser opens, or "" when it is not an
// http or https URL with a host: scheme, host (with its port) and path, the
// path's trailing slash trimmed, and nothing else of it.
func browserBase(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	clean := url.URL{Scheme: u.Scheme, Host: strings.ToLower(u.Host), Path: strings.TrimRight(u.Path, "/")}
	return clean.String()
}

// codeOwners is the owners a code host's configuration names: each entry's
// first path segment (an organization, `gophrase` of `gophrase/backend`, or a
// top-level group), resolved, folded to lower case, sorted and each once.
//
// THE OWNER AND NEVER THE REPOSITORY, which is the integration's business.
// The owner is what lets the dashboard read a bare `gophrase/backend` in prose
// as a repository and `and/or` or `src/app` as words: a two-part path is a
// repository only under an owner the company works in.
//
// A SEGMENT NO CODE HOST ALLOWS IS DROPPED rather than sent: an owner is
// letters, digits, `.`, `_` and `-`, starting with a letter or a digit
// ([ownerName]), so a reader is never handed a value no path could hold.
func codeOwners(named []string, expand func(string) string) []string {
	var out []string
	for _, entry := range named {
		owner, _, _ := strings.Cut(expand(entry), "/")
		owner = strings.ToLower(owner)
		if ownerName.MatchString(owner) && !slices.Contains(out, owner) {
			out = append(out, owner)
		}
	}
	slices.Sort(out)
	return out
}

// ownerName is a GitHub organization's or a GitLab group's path: GitHub
// allows letters, digits and hyphens, GitLab also `.` and `_`, and neither
// starts with punctuation.
var ownerName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)
