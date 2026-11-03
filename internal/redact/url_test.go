package redact_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// AN ADDRESS IS ANSWERED WITH ITS CREDENTIALS WITHHELD wherever a vendor puts
// one: a query parameter whose name marks a credential, a value the text pass
// knows the shape of under any name, a path segment of a known shape, a
// fragment's token, and a user name and password. The scheme, host and port
// stay readable, the rest keeps its order and spelling, and an address with
// nothing to withhold comes back byte for byte.
func TestAnAddressIsAnsweredWithItsCredentialsWithheld(t *testing.T) {
	t.Parallel()
	ghp := "ghp_" + strings.Repeat("b", 36)
	cases := []struct{ name, raw, want string }{
		{"no query", "https://mcp.example.com/mcp", "https://mcp.example.com/mcp"},
		{"an api key", "https://mcp.example.com/mcp?api_key=s3cr3t&region=eu",
			"https://mcp.example.com/mcp?api_key=redacted&region=eu"},
		{"names in any case, kept in order",
			"https://mcp.example.com/sse?Token=a&X-Api-Key=b&clientSecret=c&sig=d&password=e",
			"https://mcp.example.com/sse?Token=redacted&X-Api-Key=redacted&clientSecret=redacted&sig=redacted&password=redacted"},
		{"a known shape under another name", "https://mcp.example.com/mcp?q=" + ghp,
			"https://mcp.example.com/mcp?q=redacted"},
		{"a key as a path segment", "https://mcp.example.com/mcp/" + ghp + "/sse",
			"https://mcp.example.com/mcp/redacted/sse"},
		{"a fragment's token", "https://mcp.example.com/callback#access_token=opaque-value&state=xyz",
			"https://mcp.example.com/callback#access_token=redacted&state=xyz"},
		{"a fragment that is a key", "https://mcp.example.com/mcp#" + ghp, "https://mcp.example.com/mcp#redacted"},
		{"a user and password, the host and port readable", "https://jane:hunter2@mcp.example.com:8443/mcp",
			"https://redacted@mcp.example.com:8443/mcp"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := redact.URL(tc.raw); got != tc.want {
			t.Errorf("%s: URL(%q) = %q, want %q", tc.name, tc.raw, got, tc.want)
		}
	}
	// NOTHING TO WITHHOLD IS ANSWERED AS WRITTEN: no parameter reordered,
	// nothing re-encoded or normalised.
	for _, clean := range []string{
		"http://10.0.0.5:8080/mcp/v1?workspace=acme&b=2&a=1#section",
		"https://mcp.example.com/a%20b/c?q=x%2By&empty=&flag",
		// AS AN OPERATOR WROTE IT: a parse and a rebuild lower-case the
		// scheme, so only an address answered as written keeps it.
		"HTTPS://MCP.Example.com:443/MCP/?",
	} {
		if got := redact.URL(clean); got != clean {
			t.Errorf("a clean address came back changed: URL(%q) = %q", clean, got)
		}
	}
	if got := redact.URL("::not a url " + ghp); strings.Contains(got, ghp) {
		t.Errorf("an address that does not parse kept its key: %q", got)
	}
}
