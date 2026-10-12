// Package atlassianhost decides which Atlassian a Jira or Confluence address
// is on — Cloud or Data Center — and which API gateway a cloud id is read
// through.
//
// # Why this is a package rather than a helper beside its caller
//
// Three places ask the question and none of them may disagree: config
// validation (a Data Center url has to carry a webhook signing secret and a
// Cloud one does not), the Jira client (Cloud serves /rest/api/3 and names
// people by accountId, Data Center /rest/api/2 and usernames) and the
// Confluence client (a Cloud site serves the wiki under /wiki, the gateway
// and Data Center at the root). Written three times, the rule drifted: Jira
// counted every host under atlassian.com as Cloud while config and Confluence
// did not, so one company's url was Cloud to the client that read it and
// Data Center to the validator that admitted it, and the comment claiming a
// test held them equal was true only of the six hosts that test listed.
// Config sits at the bottom of the engine's import graph, so the rule lives
// here, where all three can reach it, and [Of] is the one answer they read.
//
// # The hosts, and where each comes from
//
// Atlassian runs its multi-tenant Cloud in two ENVIRONMENTS, each with its own
// site domains and its own API gateway (support.atlassian.com, "IP addresses
// and domains for Atlassian cloud apps"):
//
//   - the commercial Cloud, whose sites are served under *.atlassian.net and
//     *.jira.com ("Jira and Confluence use this") and whose gateway is
//     api.atlassian.com, read through /ex/jira/{cloudid} and
//     /ex/confluence/{cloudid} (developer.atlassian.com, "OAuth 2.0 (3LO)
//     apps");
//   - Atlassian Government Cloud, whose sites are "a unique name +
//     atlassian-us-gov-mod.net", with Confluence under /wiki like any other
//     Cloud site, and whose gateway is api.atlassian-us-gov-mod.com, read
//     through the same two paths ("Requests that use OAuth 2.0 (3LO) are made
//     via api.atlassian-us-gov-mod.com" — developer.atlassian.com, "Use OAuth
//     2.0 (3LO) on AGC-compatible apps").
//
// A host under one of those site domains is a Cloud SITE, and an environment's
// gateway host is the Cloud GATEWAY whatever path follows it — nothing on it
// is anybody's Data Center, and a path it does not serve is a 404 to fix in
// the address rather than a reason to ask for a Data Center's signing secret.
//
// atlassian.com is NOT a site domain, and that is the half Jira got wrong.
// The same page lists *.atlassian.com as what "all Atlassian apps and
// services use" — identity, administration, the gateway — and no customer's
// site is served under it. The Jira that is served there, Atlassian's own
// public tracker at jira.atlassian.com, answers /rest/api/2/serverInfo with
// "deploymentType":"Server" and does not serve /rest/api/3 at all, so calling
// it Cloud sends every request to an API it does not have. Nor is
// atlassian-us-gov-mod.com a site domain: it is Government Cloud's
// counterpart of atlassian.com, and only its gateway host is recognised.
//
// # What no host list can name, and who decides it instead
//
// The hosts above are NOT every Cloud address, and nothing here claims they
// are. Two kinds are left over:
//
//   - A CUSTOM DOMAIN, which a premium site can take for browsers. The REST
//     API is not served there (Atlassian's AX-667: "it is not possible to run
//     REST API calls using a custom domain", worked around by "the original
//     Site URL"), so an integration's url is the site's own address and the
//     custom domain belongs in a block's site_url, which is a link base and is
//     never classified.
//   - ATLASSIAN ISOLATED CLOUD, whose customers "are given their own unique
//     base domain" (the same domains page), so its sites and its gateway are
//     at hosts that are Cloud and that no suffix list could ever name.
//
// For those the address cannot answer, and the COMPANY does: [Of] takes the
// deployment it declared (integrations.atlassian.deployment) and lets that
// decide every host the lists above do not recognise. A recognised host is
// never overruled by a declaration — atlassian.net is nobody's Data Center —
// and validation refuses the company that declares otherwise. A company that
// declared nothing keeps the old answer for an unrecognised host, Data
// Center, because that is what a self-hosted instance's address looks like
// and it is the overwhelming case.
//
// Hosts compare the way DNS compares them — without case, and with an
// absolute name's trailing dot ignored; nothing here reads a path, except to
// tell a ${VAR} standing in the path from one standing where the host is.
package atlassianhost

import (
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
)

// Deployment is which Atlassian an address is on.
//
// The spellings are integrations.atlassian.deployment's, so config's own
// constants and a declaration read from a document are these values.
type Deployment string

const (
	// Unknown is an address nothing can place yet: its host is a ${VAR}
	// that only the engine resolves, in a company that declared nothing.
	// As a DECLARATION it is the company that said nothing.
	Unknown Deployment = ""
	// Cloud is Atlassian-hosted Jira and Confluence, in any environment.
	Cloud Deployment = "cloud"
	// DataCenter is self-hosted Jira and Confluence, Data Center or Server.
	DataCenter Deployment = "data_center"
)

// environment is one of Atlassian's multi-tenant Clouds: the host of its API
// gateway and the domains its sites are served under.
type environment struct {
	// gateway is the host a cloud id is read through:
	// https://<gateway>/ex/{jira|confluence}/{cloudid}.
	gateway string

	// sites are the domains this environment's Jira and Confluence sites
	// are served under, each with its leading dot, so a host IS one of their
	// subdomains rather than merely ending in the same letters
	// (evilatlassian.net is not Atlassian's).
	sites []string
}

// environments are the Clouds this package recognises, the commercial one
// first because it is what a cloud id with no site to place it in is read
// through — see the package doc for the source of every host.
var environments = []environment{
	{gateway: "api.atlassian.com", sites: []string{".atlassian.net", ".jira.com"}},
	{gateway: "api.atlassian-us-gov-mod.com", sites: []string{".atlassian-us-gov-mod.net"}},
}

// Of is the deployment the Jira or Confluence address raw is on, in a company
// that declared declared ([Unknown] for one that declared nothing).
//
// A host this package recognises — a Cloud site or a gateway — is Cloud
// whatever was declared. Any other host is what the company declared, and
// Data Center when it declared nothing. An address whose host is a ${VAR} is
// what the company declared too, and [Unknown] when it declared nothing:
// Tier B holds a reference verbatim and the engine resolves it where the
// client is built, so validation sees no host, and reading that as Data
// Center refused a Cloud company for lacking a secret only Data Center needs.
// A reference elsewhere in the address — its path — leaves the host written.
//
// A client is handed resolved addresses only, so for a client the answer is
// Cloud or Data Center; an address with no host at all is Data Center, which
// is what it always was.
func Of(raw string, declared Deployment) Deployment {
	h, written := hostOf(raw)
	switch {
	case written && isCloudHost(h):
		return Cloud
	case declared == Cloud || declared == DataCenter:
		return declared
	case written:
		return DataCenter
	default:
		return Unknown
	}
}

// IsSite reports a Cloud SITE's own address, in a recognised environment —
// the one a person opens in a browser, where Jira is served at the root and
// Confluence under /wiki.
func IsSite(raw string) bool {
	return environmentOfSite(host(raw)) != nil
}

// IsGateway reports an address on a recognised environment's API gateway,
// which already names the product in its path, so nothing is appended to it.
func IsGateway(raw string) bool {
	return isGatewayHost(host(raw))
}

// Gateway is the host of the API gateway a cloud id is read through, for a
// company whose site is at site: Government Cloud's for a Government Cloud
// site, and the commercial gateway for anything else — a commercial site, a
// custom domain, or no site at all.
//
// THE SITE IS WHAT NAMES THE ENVIRONMENT. A cloud id is an opaque identifier
// that says nothing about which Cloud issued it, and the commercial gateway
// does not serve a Government Cloud site, so the gateway follows the site
// the id belongs to.
func Gateway(site string) string {
	if env := environmentOfSite(host(site)); env != nil {
		return env.gateway
	}
	return environments[0].gateway
}

// isCloudHost reports a host this package recognises as Atlassian Cloud's.
func isCloudHost(h string) bool {
	return environmentOfSite(h) != nil || isGatewayHost(h)
}

// isGatewayHost reports one of the environments' gateway hosts.
func isGatewayHost(h string) bool {
	for _, env := range environments {
		if h == env.gateway {
			return true
		}
	}
	return false
}

// environmentOfSite is the environment whose sites are served under h's
// domain, or nil.
func environmentOfSite(h string) *environment {
	for i := range environments {
		for _, suffix := range environments[i].sites {
			if strings.HasSuffix(h, suffix) {
				return &environments[i]
			}
		}
	}
	return nil
}

// hostOf is the host raw names and whether it is WRITTEN — false when a
// ${VAR} stands where the host is, so that nothing here can know it yet.
//
// A reference anywhere else leaves the host written: in a path
// (https://jira.example.com/${CONTEXT}), in a port or in user information.
// An address with no "://" and a reference in it has its host inside that
// reference (${JIRA_URL}, ${JIRA_URL}/wiki).
func hostOf(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if !envref.Has(raw) {
		return host(raw), true
	}
	_, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return "", false
	}
	authority := rest
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}
	hostname := authority[strings.LastIndex(authority, "@")+1:]
	if strings.HasPrefix(hostname, "[") {
		if i := strings.Index(hostname, "]"); i >= 0 {
			hostname = hostname[:i+1]
		}
	} else if i := strings.Index(hostname, ":"); i >= 0 {
		hostname = hostname[:i]
	}
	if envref.Has(hostname) {
		return "", false
	}
	return host("https://" + hostname), true
}

// host is the host of an address as DNS compares it — lowercased, and without
// the trailing dot of an absolute name, which names the same host — or "" for
// an address that does not parse or names none.
func host(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
}
