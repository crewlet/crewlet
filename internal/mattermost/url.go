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

// BrowserOrigin is the Origin header a browser at base sends.
//
// SERIALISED THE WAY A BROWSER SERIALISES IT (RFC 6454 §6.2): the scheme and
// the host, lower-cased, the port only when it is not the scheme's default,
// and never a path. Each part is load-bearing, because this string is what
// Mattermost's websocket check compares and what an AllowCorsFrom entry is
// matched against: a page at https://chat.example.com:443/team sends
// `https://chat.example.com`, and a probe that kept the port or the path
// would be judged on an Origin no browser ever sends.
//
// Empty for an address no browser can be at — anything but an absolute http
// or https URL with a host.
func BrowserOrigin(base string) string {
	u, err := url.Parse(NormalizeURL(base))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if defaultPort(scheme) == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		// An IPv6 literal, which Hostname unbracketed.
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && port != defaultPort(scheme) {
		return scheme + "://" + host + ":" + port
	}
	return scheme + "://" + host
}

// defaultPort is the port a browser leaves out of an http or https Origin,
// and empty for any other scheme.
func defaultPort(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// urlPath is an address's path, without a trailing slash: the empty string
// for one served at its root.
func urlPath(raw string) string {
	u, err := url.Parse(NormalizeURL(raw))
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.Path, "/")
}
