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

// An Origin never carries a path: it is what the server's websocket check
// compares and what an allowed-origins setting is matched against, so a
// probe inventing one fails a check every real browser passes.
func TestBrowserOriginIsSchemeAndAuthorityOnly(t *testing.T) {
	for raw, want := range map[string]string{
		"https://chat.example.com":        "https://chat.example.com",
		"https://Chat.Example.COM/":       "https://chat.example.com",
		"https://example.com/chat":        "https://example.com",
		"http://localhost:8065/sub/path/": "http://localhost:8065",
		"not a url":                       "",
		"":                                "",
	} {
		if got := mattermost.BrowserOrigin(raw); got != want {
			t.Errorf("BrowserOrigin(%q) = %q, want %q", raw, got, want)
		}
	}
}

// This check exists to tell an operator their humans are blind, so a FALSE
// ALARM is worse than useless: a capitalised host and a subpath deployment
// both work perfectly, and a whole-URL compare calls both broken.
func TestOriginMatchingModelsTheServersOwnRule(t *testing.T) {
	cases := []struct {
		name                 string
		configured, reported string
		want                 bool
	}{
		{"identical", "https://chat.example.com", "https://chat.example.com", true},
		{"a trailing slash", "https://chat.example.com/", "https://chat.example.com", true},
		{"a capitalised host", "https://Chat.Example.com", "https://chat.example.com", true},
		{"a subpath deployment", "https://example.com/chat", "https://example.com/chat/", true},
		// The path is not part of an Origin, so these two DO pass the
		// websocket check — even though the paths disagree.
		{"a path disagreement", "https://example.com/chat", "https://example.com", true},
		// These are the real failures: a different host, and the
		// http/https mix-up that silently blinds every human.
		{"a different host", "https://chat.example.com", "https://other.example.com", false},
		{"a scheme mismatch", "https://chat.example.com", "http://chat.example.com", false},
		{"a port mismatch", "http://localhost:8065", "http://localhost:9065", false},
		{"nothing configured", "", "https://chat.example.com", false},
		{"nothing reported", "https://chat.example.com", "", false},
	}
	for _, c := range cases {
		if got := mattermost.OriginMatches(c.configured, c.reported); got != c.want {
			t.Errorf("%s: OriginMatches(%q, %q) = %v, want %v",
				c.name, c.configured, c.reported, got, c.want)
		}
	}
}

// A separate question with a separate consequence: a wrong path does not
// break the websocket, it breaks every absolute link the server builds.
func TestPathMatchingIsItsOwnQuestion(t *testing.T) {
	if !mattermost.PathMatches("https://example.com/chat", "https://example.com/chat/") {
		t.Error("two spellings of one subpath disagree")
	}
	if mattermost.PathMatches("https://example.com/chat", "https://example.com") {
		t.Error("a path disagreement was not reported")
	}
	// And it is orthogonal to the origin: a host mismatch on the same
	// path is a socket problem, not a link problem.
	if !mattermost.PathMatches("https://a.example.com/chat", "https://b.example.com/chat") {
		t.Error("the path check reported a host difference")
	}
}
