package searchconsole

import (
	"net/url"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// The two kinds of Site Verification resource, as its API types them.
const (
	verifySite   = "SITE"
	verifyDomain = "INET_DOMAIN"
)

// domainPrefix starts a Domain property.
const domainPrefix = "sc-domain:"

// covers reports whether page is a page of site: one a URL prefix starts, as
// Search Console compares them, or one whose host is a domain or below it.
// The page is compared parsed, never as a string, and one a server could
// resolve elsewhere is no page of any site ([pageURL]).
func covers(site, page string) bool {
	u, ok := pageURL(page)
	if !ok {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if domain, ok := strings.CutPrefix(site, domainPrefix); ok {
		return host == domain || strings.HasSuffix(host, "."+domain)
	}
	s, err := url.Parse(site)
	if err != nil || !strings.HasSuffix(s.EscapedPath(), "/") {
		return false
	}
	return strings.EqualFold(u.Scheme, s.Scheme) && strings.EqualFold(u.Host, s.Host) &&
		strings.HasPrefix(u.EscapedPath(), s.EscapedPath())
}

// pageURL is page parsed, refused where a server could resolve it to another
// path than it spells: a dot segment, raw or percent-encoded at any depth, or
// a backslash, which servers read as a slash.
func pageURL(page string) (*url.URL, bool) {
	u, err := url.Parse(page)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		strings.ContainsRune(page, '\\') {
		return nil, false
	}
	path := u.EscapedPath()
	for range 4 {
		if strings.ContainsRune(path, '\\') ||
			slices.ContainsFunc(strings.Split(path, "/"), func(seg string) bool { return seg == "." || seg == ".." }) {
			return nil, false
		}
		decoded, err := url.PathUnescape(path)
		if err != nil {
			return nil, false
		}
		if decoded == path {
			return u, true
		}
		path = decoded
	}
	// STILL ENCODED AFTER FOUR DECODINGS: no page of a site needs that.
	return nil, false
}

// coverOf is the site of sites page belongs to, the narrowest where several
// do: the longest URL prefix, else the longest domain.
func coverOf(sites []string, page string) (string, bool) {
	best := ""
	for _, s := range sites {
		if !covers(s, page) {
			continue
		}
		if best == "" || narrower(s, best) {
			best = s
		}
	}
	return best, best != ""
}

// narrower reports whether site a is narrower than b: a URL prefix is
// narrower than any domain, and a longer one than a shorter.
func narrower(a, b string) bool {
	aDomain, bDomain := strings.HasPrefix(a, domainPrefix), strings.HasPrefix(b, domainPrefix)
	if aDomain != bDomain {
		return !aDomain
	}
	return len(a) > len(b)
}

// verifiedSite is the property a Site Verification resource of type
// verifies: a domain's sc-domain property, or a site's URL prefix, whose
// trailing slash Site Verification may leave off.
func verifiedSite(kind, identifier string) (string, bool) {
	var site string
	switch kind {
	case verifyDomain:
		site = domainPrefix + identifier
	case verifySite:
		site = identifier
		if u, err := url.Parse(identifier); err == nil && u.Path == "" {
			site += "/"
		}
	default:
		return "", false
	}
	return site, config.SearchConsoleSite(site)
}

// resourceSite is the property a Site Verification resource ID names:
// dns://example.com for a domain, the site's URL for a URL prefix. Google
// hands an ID back percent-encoded, so an encoded one reads the same.
func resourceSite(id string) (string, bool) {
	if strings.Contains(id, "%") {
		decoded, err := url.PathUnescape(id)
		if err != nil {
			return "", false
		}
		id = decoded
	}
	if domain, ok := strings.CutPrefix(id, "dns://"); ok {
		return verifiedSite(verifyDomain, domain)
	}
	return verifiedSite(verifySite, id)
}

// resourceOf is the Site Verification resource of site: its type, its
// identifier and the ID Site Verification names it by.
func resourceOf(site string) (kind, identifier, id string) {
	if domain, ok := strings.CutPrefix(site, domainPrefix); ok {
		return verifyDomain, domain, "dns://" + domain
	}
	return verifySite, site, site
}

// bodySite is the property a Site Verification body's `site` names.
func bodySite(body map[string]any) (string, bool) {
	s, _ := body["site"].(map[string]any)
	kind, _ := s["type"].(string)
	identifier, _ := s["identifier"].(string)
	return verifiedSite(kind, identifier)
}

// methodFits reports whether verification method checks a property of kind:
// a DNS record proves a domain, everything else a URL prefix.
func methodFits(kind, method string) bool {
	dns := strings.HasPrefix(method, "DNS")
	return slices.Contains(verificationMethods, method) && dns == (kind == verifyDomain)
}

// absoluteURL reports whether s is an http or https URL with a host and no
// credentials: a page or a sitemap.
func absoluteURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}
