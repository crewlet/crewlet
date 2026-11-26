package webhooks

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"github.com/crewlet/crewlet/internal/atlassianhost"
)

// A FORGE TOKEN IS BOUND TO THE COMPANY'S OWN SITE, not only to its app.
//
// One app id is shared by every site that installs the app, so the audience
// says which app a token is for and nothing about whose site. The token's
// signed claims name its installation's site by cloud id only, never by host
// (Atlassian's Forge Invocation Token reference: app.installation.contexts[]
// carry a cloudId and an apiBaseUrl of the form
// https://api.atlassian.com/ex/{product}/{cloudId}, and so does
// app.apiBaseUrl). So the company's site is a cloud id too: its `cloud_id`, or
// the one a gateway `url` carries. A Cloud site named only by its host has no
// cloud id to compare, and its product's relayed events are held. The body's
// `context` is never read for any of this: the token does not sign it.

// cloudIDOf is the cloud id one product block names, lower-cased, or "": a
// block naming a Data Center instance, a Cloud site by its host, or a site the
// Atlassian organization has not supplied yet.
func cloudIDOf(rawURL, cloudID string) string {
	if id := strings.ToLower(strings.TrimSpace(cloudID)); id != "" {
		return id
	}
	if rawURL = strings.TrimSpace(rawURL); atlassianhost.IsGateway(rawURL) {
		return gatewayCloudID(rawURL)
	}
	return ""
}

// gatewayCloudID is the cloud id in a gateway address's /ex/{product}/{id}
// path, lower-cased, or "" for any other shape.
func gatewayCloudID(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "ex" || (parts[1] != "jira" && parts[1] != "confluence") {
		return ""
	}
	return strings.ToLower(parts[2])
}

// forgeSource is the product an event name belongs to, "" for neither.
// Decided by prefix rather than by [forgeEvents], so an event this build does
// not map is still held to its product's site.
func forgeSource(event string) string {
	switch {
	case strings.HasPrefix(event, "avi:jira:"):
		return "jira"
	case strings.HasPrefix(event, "avi:confluence:"):
		return "confluence"
	}
	return ""
}

// forgeSites are the cloud ids a token for an event of source may name: that
// product's, or for an event of neither product either one's.
func (s Secrets) forgeSites(source string) []string {
	var out []string
	for _, product := range []string{"jira", "confluence"} {
		if id, ok := s.ForgeSites[product]; ok && (source == "" || source == product) {
			out = append(out, id)
		}
	}
	return out
}

// forgeClaims are the invocation token's claims this edge reads: the
// registered ones the parser validates, and the site its installation is on.
type forgeClaims struct {
	jwt.RegisteredClaims
	App struct {
		APIBaseURL   string `json:"apiBaseUrl"`
		Installation struct {
			Contexts []struct {
				CloudID    string `json:"cloudId"`
				APIBaseURL string `json:"apiBaseUrl"`
			} `json:"contexts"`
		} `json:"installation"`
	} `json:"app"`
}

var (
	// errNoSite is a verified token naming no Jira or Confluence site.
	errNoSite = errors.New("the token names no Jira or Confluence site")
	// errSiteMismatch is a verified token minted for an installation on a
	// site the company did not configure.
	errSiteMismatch = errors.New("the token was minted for another site's installation of the app")
)

// site is the one cloud id the token's claims name. An installation is on
// one site, so every claim naming a cloud id must name the same one; a token
// whose claims disagree is refused rather than matched on whichever agrees.
func (c *forgeClaims) site() (string, error) {
	named := map[string]bool{}
	add := func(id string) {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			named[id] = true
		}
	}
	add(gatewayCloudID(c.App.APIBaseURL))
	for _, installed := range c.App.Installation.Contexts {
		add(installed.CloudID)
		add(gatewayCloudID(installed.APIBaseURL))
	}
	ids := make([]string, 0, len(named))
	for id := range named {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	switch len(ids) {
	case 0:
		return "", errNoSite
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("the token's claims name %d sites (%s), and one installation is on one",
		len(ids), strings.Join(ids, ", "))
}

// bindSite holds a verified token to one of the company's cloud ids.
func bindSite(claims *forgeClaims, sites []string) error {
	site, err := claims.site()
	if err != nil {
		return err
	}
	if slices.Contains(sites, site) {
		return nil
	}
	return fmt.Errorf("%w: it names cloud id %s, and integrations.jira or "+
		"integrations.confluence names %s", errSiteMismatch, site, strings.Join(sites, ", "))
}
