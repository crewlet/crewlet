package mattermost_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/mattermost"
)

func TestNormalizeURLMakesOneAddressComparable(t *testing.T) {
	for _, raw := range []string{
		"https://chat.example.com",
		"https://chat.example.com/",
		"https://chat.example.com///",
		"  https://chat.example.com/  ",
	} {
		if got := mattermost.NormalizeURL(raw); got != "https://chat.example.com" {
			t.Errorf("NormalizeURL(%q) = %q", raw, got)
		}
	}
	// A subpath deployment keeps its path — that IS the address.
	if got := mattermost.NormalizeURL("https://example.com/chat/"); got != "https://example.com/chat" {
		t.Errorf("a subpath was normalised to %q", got)
	}
}

// A plaintext socket against an https instance fails at a layer the
// transport reads as "the server is down".
func TestTheWebsocketSchemeIsUpgraded(t *testing.T) {
	for raw, want := range map[string]string{
		"https://chat.example.com":  "wss://chat.example.com/api/v4/websocket",
		"http://localhost:8065":     "ws://localhost:8065/api/v4/websocket",
		"https://example.com/chat/": "wss://example.com/chat/api/v4/websocket",
	} {
		if got := mattermost.WebsocketURL(raw); got != want {
			t.Errorf("WebsocketURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

// THE ORIGIN A BROWSER SENDS, serialised as a browser serialises it: never a
// path, lower-cased, and no port when it is the scheme's default. It is what
// the server's websocket check compares and what an AllowCorsFrom entry is
// matched against, so a probe that kept the path or the port would be judged
// on an Origin no browser ever sends.
func TestBrowserOriginIsWhatABrowserSends(t *testing.T) {
	for raw, want := range map[string]string{
		"https://chat.example.com":        "https://chat.example.com",
		"https://Chat.Example.COM/":       "https://chat.example.com",
		"HTTPS://chat.example.com":        "https://chat.example.com",
		"https://example.com/chat":        "https://example.com",
		"http://localhost:8065/sub/path/": "http://localhost:8065",
		"https://chat.example.com:443":    "https://chat.example.com",
		"http://chat.example.com:80/":     "http://chat.example.com",
		"https://chat.example.com:80":     "https://chat.example.com:80",
		"http://[::1]:8065":               "http://[::1]:8065",
		// Nothing a browser can be at.
		"localhost:8065":         "",
		"chat.example.com:8065":  "",
		"/mattermost":            "",
		"http://":                "",
		"ftp://chat.example.com": "",
		"not a url":              "",
		"":                       "",
	} {
		if got := mattermost.BrowserOrigin(raw); got != want {
			t.Errorf("BrowserOrigin(%q) = %q, want %q", raw, got, want)
		}
	}
}

// THE PATH IS THE ONE PART OF THE ADDRESS THE ENGINE SHARES WITH THE SITE
// URL: the server routes under the Site URL's path whatever host a request
// arrives on, so the host may differ and the path may not.
func TestPathMatchingIgnoresTheHost(t *testing.T) {
	if !mattermost.PathMatches("https://example.com/chat", "https://example.com/chat/") {
		t.Error("two spellings of one subpath disagree")
	}
	if mattermost.PathMatches("https://example.com/chat", "https://example.com") {
		t.Error("a path disagreement was not reported")
	}
	if !mattermost.PathMatches("http://mattermost.chat.svc.cluster.local:8065/chat",
		"https://chat.example.com/chat") {
		t.Error("an in-cluster address under the Site URL's path was reported")
	}
}
