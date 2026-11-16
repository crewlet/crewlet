package config

import (
	"errors"
	"strings"
	"testing"
)

// A negative MCP timeout is refused in a person's words too: the field named
// as the wizard labels it, the default offered, no path, key or value.
func TestANegativeMCPTimeoutIsRefusedInAPersonsWords(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ field, named string }{
		{"startup_timeout_seconds", "start timeout"},
		{"request_timeout_seconds", "call timeout"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			doc := `
name: Acme
providers: {sandbox: {fake: true}}
mcp_servers:
  - {name: search, command: search-mcp, ` + tc.field + `: -5}
roles:
  - {name: CEO, handle: ceo}
`
			path := "mcp_servers[0]." + tc.field
			err := rejects(t, doc, path)
			if !errors.Is(err, ErrOutOfRange) {
				t.Fatalf("want ErrOutOfRange, got %v", err)
			}
			var found bool
			for _, p := range Problems(err) {
				if p.Path != path {
					continue
				}
				found = true
				if !strings.Contains(p.Said, tc.named) || !strings.Contains(p.Said, "empty for the default") {
					t.Errorf("said = %q, want the %s named and the default offered", p.Said, tc.named)
				}
				for _, operator := range []string{"mcp_servers", tc.field, "-5", "out of range"} {
					if strings.Contains(p.Said, operator) {
						t.Errorf("said = %q carries the operator's %q", p.Said, operator)
					}
				}
			}
			if !found {
				t.Fatalf("no problem at %s in %v", path, err)
			}
		})
	}
}
