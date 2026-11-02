package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/mcp/catalogue"
)

// oauthDoc is a company with one remote server whose entry carries lines.
func oauthDoc(lines string) string {
	return `
name: Acme
mcp_servers:
  - name: linear
    transport: http
    url: https://mcp.linear.app/mcp
` + lines
}

// EACH REGISTRATION NAMES WHAT IT NEEDS, AND ONLY THAT: the company's own
// client needs its id and may carry a secret, and a published document or a
// registration names neither, since the id is the engine's document address or
// what the registration issues.
func TestASignInNamesWhatItsRegistrationNeeds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, lines string
	}{
		{"published document", "    oauth:\n      client: cimd\n"},
		{"registration with scopes", "    oauth:\n      client: dcr\n      scopes: [read, \"write:issues\"]\n"},
		{"own public client", "    oauth:\n      client: own\n      client_id: crewlet-acme\n"},
		{"own confidential client", "    oauth:\n      client: own\n      client_id: crewlet-acme\n" +
			"      client_secret: ${LINEAR_CLIENT_SECRET}\n"},
	} {
		cfg := mustCompany(t, oauthDoc(tc.lines))
		if cfg.MCPServers[0].OAuth == nil || !cfg.MCPServers[0].OAuth.Client.Valid() {
			t.Errorf("%s: parsed oauth %+v", tc.name, cfg.MCPServers[0].OAuth)
		}
	}
	for _, tc := range []struct {
		name, lines, path string
		want              error
	}{
		{"no client", "    oauth:\n      scopes: [read]\n", "mcp_servers[0].oauth.client", ErrMissing},
		{"unknown client", "    oauth:\n      client: device\n", "mcp_servers[0].oauth.client", ErrUnknownValue},
		{"own with no id", "    oauth:\n      client: own\n", "mcp_servers[0].oauth.client_id", ErrMissing},
		{"cimd with an id", "    oauth:\n      client: cimd\n      client_id: x\n",
			"mcp_servers[0].oauth.client_id", ErrConflict},
		{"dcr with a secret", "    oauth:\n      client: dcr\n      client_secret: ${S}\n",
			"mcp_servers[0].oauth.client_secret", ErrConflict},
		{"a scope with a space", "    oauth:\n      client: cimd\n      scopes: [\"read write\"]\n",
			"mcp_servers[0].oauth.scopes[0]", ErrUnknownValue},
		{"a scope twice", "    oauth:\n      client: cimd\n      scopes: [read, read]\n",
			"mcp_servers[0].oauth.scopes[1]", ErrConflict},
	} {
		err := rejects(t, oauthDoc(tc.lines), tc.path)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: refused with %v, want %v", tc.name, err, tc.want)
		}
	}
	many := make([]string, MaxMCPOAuthScopes+1)
	for i := range many {
		many[i] = fmt.Sprintf("s%d", i)
	}
	rejects(t, oauthDoc("    oauth:\n      client: cimd\n      scopes: ["+strings.Join(many, ", ")+"]\n"),
		"mcp_servers[0].oauth.scopes")
}

// A SIGN-IN IS THE COMPANY'S, FOR A REMOTE SERVER, AND SENDS AUTHORIZATION
// ITSELF: a stdio server, a per-agent server and a configured Authorization
// header are each refused beside it, naming what to change.
func TestASignInIsTheCompanysOneGrantForARemoteServer(t *testing.T) {
	t.Parallel()
	err := rejects(t, `
name: Acme
mcp_servers:
  - name: local
    command: local-mcp
    oauth:
      client: cimd
`, "mcp_servers[0].oauth")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "stdio") {
		t.Errorf("a stdio server with oauth: %v", err)
	}
	err = rejects(t, oauthDoc("    shared: false\n    oauth:\n      client: cimd\n"), "mcp_servers[0].oauth")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "mcp_env") {
		t.Errorf("a per-agent server with oauth: %v", err)
	}
	err = rejects(t, oauthDoc("    headers:\n      authorization: Bearer ${T}\n    oauth:\n      client: cimd\n"),
		"mcp_servers[0].headers.authorization")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("an Authorization header beside oauth: %v", err)
	}
	// ANOTHER HEADER RIDES BESIDE THE SIGN-IN.
	cfg := mustCompany(t, oauthDoc("    headers:\n      X-Org: acme\n    oauth:\n      client: cimd\n"))
	if cfg.MCPServers[0].OAuth.Client != catalogue.ClientCIMD {
		t.Fatalf("parsed %+v", cfg.MCPServers[0].OAuth)
	}
}

// A BOX IS NEVER HANDED A SERVER SIGNED IN TO WITH OAUTH: it is given a
// server's credentials once, at its launch, and only the engine holds what
// renews an access token, so the entry is refused and the box reaches the
// server through the bridge. The server runs every tool at allow, as any
// server a box lists must, so the sign-in is the one thing refused.
func TestABoxIsNeverHandedAServerSignedInToWithOAuth(t *testing.T) {
	t.Parallel()
	doc := oauthDoc("    tools: {default: allow}\n    oauth:\n      client: cimd\n") + `
roles:
  - name: Engineer
    handle: eng
    sandbox:
      mcp:
        servers: [linear]
`
	err := rejects(t, doc, "sandbox.mcp.servers[0]")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "bridge") {
		t.Fatalf("a box listing a signed-in server: %v", err)
	}
	mustCompany(t, strings.Replace(doc, "    oauth:\n      client: cimd\n", "", 1))
}
