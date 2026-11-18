package mattermost

import (
	"fmt"
	"net/url"
)

// What a server's Site URL can get wrong, judged by what the server itself
// does with the value.
//
// Never by comparing it with the address the engine dials: people browse the
// Site URL while the engine may dial an in-cluster Service, and the engine sends
// no Origin ([dialWebsocket]); that comparison fired on correct split-horizon
// setups. The Site URL feeds the origin check (v11.11.1): unless AllowCorsFrom
// is set a browser's Origin host and scheme must equal it (none: the request's
// host). So the faults wrong whatever is dialled are no Site URL, and one no
// browser matches: not absolute http(s), which validation admits
// (`chat.example.com:8065` parses as a scheme), or spelling its default port.
// AllowCorsFrom faults are the doctor's probe's; paths are observed per call.

// SiteURLFault is one thing wrong with a server's Site URL.
type SiteURLFault struct {
	// Event names it in the engine's log.
	Event string
	// Detail says what breaks and what to change — the one sentence the
	// transport logs and the doctor prints, so the two never disagree.
	Detail string
}

// The events a [SiteURLFault] is logged under.
const (
	// EventSiteURLUnset is a server with no Site URL.
	EventSiteURLUnset = "mattermost_site_url_unset"
	// EventSiteURLRefusesBrowsers is a Site URL whose own browsers the
	// server's origin check refuses.
	EventSiteURLRefusesBrowsers = "mattermost_site_url_refuses_browsers"
)

// JudgeSiteURL reports what is wrong with the Site URL a server reports, and
// false when nothing is.
func JudgeSiteURL(reported string) (SiteURLFault, bool) {
	site := NormalizeURL(reported)
	switch {
	case site == "":
		// Mattermost logs "SiteURL must be set" at every start and lists
		// the consequences in its own documentation; the live feed is
		// not among them, because with no Site URL the origin check
		// compares the Origin with the request's Host header. That holds
		// only where a proxy in front forwards the Host a browser sent,
		// which Mattermost's documented nginx configuration does
		// (proxy_set_header Host $http_host) and nginx's own default does
		// not (it sends the upstream's), so the sentence says so.
		return SiteURLFault{Event: EventSiteURLUnset, Detail: "ServiceSettings.SiteURL " +
			"is unset. Mattermost builds every absolute address it hands out " +
			"from it, so the links in its notification emails are broken, " +
			"email batching does not run and OAuth sign-in fails. Browsers " +
			"keep their live feed wherever whatever is in front of the server " +
			"forwards the Host they typed, as Mattermost's documented proxy " +
			"configuration does: with no Site URL the server admits a " +
			"websocket whose Origin names the host in the request's Host " +
			"header. Set it to the address people type into their browser, " +
			"in System Console → Environment → Web Server"}, true
	case BrowserOrigin(site) == "":
		return SiteURLFault{Event: EventSiteURLRefusesBrowsers, Detail: fmt.Sprintf(
			"ServiceSettings.SiteURL is %q, which is not an absolute http or "+
				"https address, so no browser's Origin can match it: the server "+
				"refuses every person's websocket (unless AllowCorsFrom admits "+
				"their origin), so nobody sees a message until they reload, and "+
				"every link it builds from the value is broken. Mattermost's own "+
				"validation accepts it, so nothing else reports it, and the "+
				"agents keep working because their sockets carry no Origin. Set "+
				"it to the address people type into their browser, scheme "+
				"included", site)}, true
	case spellsDefaultPort(site):
		return SiteURLFault{Event: EventSiteURLRefusesBrowsers, Detail: fmt.Sprintf(
			"ServiceSettings.SiteURL %s spells out its scheme's default port. "+
				"A browser never sends a default port in its Origin, and the "+
				"server compares the Origin's host with the Site URL's, port "+
				"included, so it refuses every person's websocket (unless "+
				"AllowCorsFrom admits their origin) and nobody sees a message "+
				"until they reload, while the agents, whose sockets carry no "+
				"Origin, keep working. Remove the port", site)}, true
	}
	return SiteURLFault{}, false
}

// spellsDefaultPort reports a Site URL that writes out its scheme's default
// port — the one difference between it and its browsers' Origin that the
// server's host comparison does not forgive.
func spellsDefaultPort(site string) bool {
	u, err := url.Parse(site)
	if err != nil {
		return false
	}
	port := u.Port()
	return port != "" && port == defaultPort(u.Scheme)
}
