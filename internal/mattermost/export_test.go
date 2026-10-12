package mattermost

import (
	"net/url"
	"slices"
	"strings"
)

// AdmitsOrigin is Mattermost's websocket origin rule, transcribed from its
// newest release, v11.11.1: App.OriginChecker (server/channels/app/server.go)
// and utils.CheckOrigin (server/channels/utils/api.go). origin is the
// upgrade's Origin header, empty for none; host is the Host it was sent to.
//
// A TRANSCRIPTION RATHER THAN A FAKE'S OWN IDEA, so the suite holds the
// engine's sockets, the doctor's probes and [JudgeSiteURL] to what the server
// does. The engine once sent an Origin because its code and its tests
// shared one wrong belief about this rule, and a twin that agrees only with
// itself certifies nothing.
func AdmitsOrigin(origin, host, siteURL, allowCorsFrom string) bool {
	if allowCorsFrom != "" {
		allowed := allowCorsFrom
		if allowed != "*" {
			site, err := url.Parse(siteURL)
			if err == nil {
				site.Path = ""
				allowed += " " + site.String()
			}
		}
		// utils.CheckOrigin.
		if origin == "" || allowed == "*" {
			return true
		}
		return slices.Contains(strings.Split(allowed, " "), origin)
	}
	// The default checker the server installs instead of the library's.
	if origin == "" {
		return true
	}
	if origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if siteURL == "" {
		return strings.EqualFold(u.Host, host)
	}
	site, err := url.Parse(siteURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, site.Host) && strings.EqualFold(u.Scheme, site.Scheme)
}
