package mattermost

import (
	"fmt"
	"net/url"
)

// What a server's Site URL can get wrong, judged by what the server itself
// does with the value.
//
// # Never by comparing it with the address the engine dials
//
// That comparison is what this replaced, and its premise was false twice
// over. Browsers do not use the engine's address: people browse the Site URL,
// and the engine may reach the same server by any route — an in-cluster
// Service while people browse a public name is the ordinary deployment on
// Kubernetes, not a mistake. And Mattermost's websocket origin check is a
// browser control the engine is not subject to: it sends no Origin header
// (see [dialWebsocket]), and every branch of the check admits an upgrade that
// carries none. So "the two addresses differ" predicted nothing, and a
// warning saying browsers would fail fired on every correctly configured
// split-horizon deployment.
//
// # What the server does with it (v11.11.1)
//
// The Site URL is where every absolute address the server hands out starts,
// and the input to its websocket origin check (App.OriginChecker,
// server/channels/app/server.go). With AllowCorsFrom unset, a browser's
// upgrade is admitted when its Origin's host and scheme equal the Site URL's
// — or, with no Site URL, the host the request was sent to. With
// AllowCorsFrom set, the Origin must equal one of its entries or the Site
// URL with its path removed, string for string (utils.CheckOrigin). An
// upgrade with no Origin is admitted either way.
//
// So what is observable from the public configuration, and wrong whatever
// address anybody dials, is two things: no Site URL at all, and one no
// browser can match — not an absolute http(s) address, which the server's own
// validation lets through (url.ParseRequestURI takes `chat.example.com:8065`
// as a scheme and an opaque rest, and `/mattermost` as a path), or one that
// spells out its scheme's default port, which no browser's Origin carries.
// AllowCorsFrom is not in the public configuration, so a fault that hangs on
// it is the doctor's live browser probe to find, never this judgement's.
//
// # Nor the engine's path
//
// Mattermost serves its API under its Site URL's path whatever host a
// request arrives on, so an engine address under another path misses it —
// but comparing the two paths predicts that wrongly in both directions: a
// proxy that maps a path of its own onto the server's is fine and differs,
// and a proxy that redirects http to https breaks every write and does not.
// What happens instead is observed where it happens, on every call: the
// client refuses a redirect, naming the address the server sent it to, and
// names a web page answered where the API's JSON should be (client.go).

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
		// compares the Origin with the host the request was sent to.
		return SiteURLFault{Event: EventSiteURLUnset, Detail: "ServiceSettings.SiteURL " +
			"is unset. Mattermost builds every absolute address it hands out " +
			"from it, so the links in its notification emails are broken, " +
			"email batching does not run and OAuth sign-in fails. Browsers " +
			"keep their live feed: with no Site URL the server admits a " +
			"websocket whose Origin names the host it was sent to. Set it to " +
			"the address people type into their browser, in System Console " +
			"→ Environment → Web Server"}, true
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
