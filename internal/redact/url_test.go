package redact_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// AN ADDRESS IS ANSWERED WITH ITS CREDENTIALS WITHHELD: a query parameter
// whose name marks a credential, a value the text pass knows the shape of
// under any name, and a user name and password, while everything a reader
// needs to recognise the server (scheme, host, path, the other parameters)
// stays as written.
func TestAnAddressIsAnsweredWithItsCredentialsWithheld(t *testing.T) {
	t.Parallel()
	ghp := "ghp_" + strings.Repeat("b", 36)
	cases := []struct{ name, raw, want string }{
		{"no query", "https://mcp.example.com/mcp", "https://mcp.example.com/mcp"},
		{"an api key", "https://mcp.example.com/mcp?api_key=s3cr3t&region=eu",
			"https://mcp.example.com/mcp?api_key=redacted&region=eu"},
		{"names in any case", "https://mcp.example.com/sse?Token=a&X-Api-Key=b&clientSecret=c&sig=d&password=e",
			"https://mcp.example.com/sse?Token=redacted&X-Api-Key=redacted&clientSecret=redacted&password=redacted&sig=redacted"},
		{"a known shape under another name", "https://mcp.example.com/mcp?q=" + ghp,
			"https://mcp.example.com/mcp?q=redacted"},
		{"a user and password", "https://jane:hunter2@mcp.example.com/mcp", "https://redacted@mcp.example.com/mcp"},
		{"nothing to withhold", "http://10.0.0.5:8080/mcp?workspace=acme", "http://10.0.0.5:8080/mcp?workspace=acme"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := redact.URL(tc.raw); got != tc.want {
			t.Errorf("%s: URL(%q) = %q, want %q", tc.name, tc.raw, got, tc.want)
		}
	}
	if got := redact.URL("::not a url " + ghp); strings.Contains(got, ghp) {
		t.Errorf("an address that does not parse kept its key: %q", got)
	}
}
