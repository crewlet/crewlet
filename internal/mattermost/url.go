package mattermost

import (
	"net/url"
	"strings"
)

// The URL derivations, in ONE place.
//
// They were written out three times in the engine this replaces — the config
// model, the transport building the socket fleet, and the doctor — and a
// divergence between them is invisible until an https:// instance silently
// gets a plaintext ws:// socket.

// WebsocketPath is Mattermost's websocket endpoint.
const WebsocketPath = "/api/v4/websocket"

// APIPath is the REST prefix every call hangs off.
const APIPath = "/api/v4"

// NormalizeURL is the instance address in the one shape every consumer
// compares against.
//
// Trailing slashes go, because this value is BOTH string-compared against
// the server's own reported Site URL and concatenated with API paths —
// https://chat.example/ and https://chat.example are the same address, and
// only one of them produces a working path when something is appended.
func NormalizeURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// WebsocketURL is the websocket endpoint for an instance.
//
// The scheme is upgraded rather than assumed: a plaintext socket against an
// https instance fails in a way nothing reports, because the connection is
// refused at a layer the transport reads as "the server is down".
func WebsocketURL(base string) string {
	b := NormalizeURL(base)
	switch {
	case strings.HasPrefix(b, "https://"):
		b = "wss://" + strings.TrimPrefix(b, "https://")
	case strings.HasPrefix(b, "http://"):
		b = "ws://" + strings.TrimPrefix(b, "http://")
	}
	return b + WebsocketPath
}

// BrowserOrigin is the Origin header a browser at base would send.
//
// SCHEME AND AUTHORITY ONLY, lower-cased. An Origin never carries a path,
// which matters twice: it is what Mattermost's websocket check compares, and
// it is the exact string an allowed-origins setting is matched against — so
// a probe inventing a path-bearing Origin fails a check every real browser
// passes.
func BrowserOrigin(base string) string {
	u, err := url.Parse(NormalizeURL(base))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// OriginMatches reports whether a browser at configured would pass the
// server's websocket origin check against the Site URL it reports.
//
// THIS MODELS THE SERVER'S OWN RULE rather than approximating it: it
// compares host and scheme case-insensitively and ignores the path entirely.
// A whole-URL string compare reports a mismatch for a capitalised host and
// for every subpath deployment, on installs whose live feed works perfectly
// — and this check exists to tell an operator their humans are blind, so a
// false alarm is worse than useless.
func OriginMatches(configured, reported string) bool {
	left, right := BrowserOrigin(configured), BrowserOrigin(reported)
	return left != "" && left == right
}

// PathMatches reports whether the two agree on the subpath the server is
// served under.
//
// A SEPARATE QUESTION from the origin, with a separate consequence: a wrong
// path does not break the websocket, it breaks every absolute link and
// plugin URL the server builds. Reporting them together would send an
// operator to fix a socket that works.
func PathMatches(configured, reported string) bool {
	return urlPath(configured) == urlPath(reported)
}

func urlPath(raw string) string {
	u, err := url.Parse(NormalizeURL(raw))
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.Path, "/")
}
