package seataccess_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A PER-SEAT SERVER THAT READS NO AGENT'S CREDENTIAL IS REPORTED by its
// integration's sync: it would start, and act as nobody.
func TestASyncReportsAToolDeclaredWithNoCredentialBinding(t *testing.T) {
	t.Parallel()
	h := newHarness()
	c := company(&config.RoleDatadog{Enabled: yes(), Role: "a"})
	c.MCPServers = []config.MCPServer{
		{Name: "datadog", Transport: config.TransportStdio, Command: "my-datadog-server", Shared: org.Off()},
	}
	report := h.sync(t, c, nil)
	var found *integration.Finding
	for i := range report.Findings {
		if report.Findings[i].Subject == "mcp_servers.datadog" {
			found = &report.Findings[i]
		}
	}
	if found == nil || found.Kind != integration.FindingCredentialMissing || found.Remedy == "" {
		t.Fatalf("findings = %+v, want the unbound server named", report.Findings)
	}

	// RECOGNISED, OR DECLARED, it is bound and not reported.
	c.MCPServers[0].SeatCredentials = []config.SeatCredential{{Kind: "datadog", Key: "DD_KEY"}}
	for _, f := range newHarness().sync(t, c, nil).Findings {
		if f.Subject == "mcp_servers.datadog" {
			t.Errorf("a declared server was reported: %+v", f)
		}
	}
}

// THE TOOL STATE COMES FROM THE RESOLVED BINDING, not from a server's name:
// a recognised server under any name is the seat's tool, a server reading
// another integration's credential is not, and Jira and Confluence are the
// engine's own tools until a server reads the Atlassian credential.
func TestTheToolStateComesFromTheResolvedBinding(t *testing.T) {
	t.Parallel()
	server := func(name, command string) config.MCPServer {
		return config.MCPServer{Name: name, Transport: config.TransportStdio, Command: command, Shared: org.Off()}
	}
	live := held("a")
	cases := []struct {
		name string
		c    *config.Company
		lane seataccess.Lane
		want seataccess.ToolState
	}{
		{"a recognised server under another name", &config.Company{MCPServers: []config.MCPServer{
			server("chat", "slack-mcp-server")}}, seataccess.LaneSlack, seataccess.ToolLive},
		{"a server reading another integration", &config.Company{MCPServers: []config.MCPServer{
			server("chat", "slack-mcp-server")}}, seataccess.LaneMattermost, seataccess.ToolNoServer},
		{"Jira on the engine's own tools", &config.Company{}, seataccess.LaneJira, seataccess.ToolBuiltin},
		{"Jira through mcp-atlassian", &config.Company{MCPServers: []config.MCPServer{
			server("atl", "mcp-atlassian")}}, seataccess.LaneJira, seataccess.ToolLive},
	}
	for _, tc := range cases {
		if got := seataccess.ToolStateOf(tc.c, tc.lane, on("a"), live); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// THE KEYS A RECOGNISED SERVER READS ARE THE VENDOR'S CREDENTIAL KEYS, so the
// document rules refuse a writer naming them in a seat's mcp_env and the
// migration finds them there: the binding table sits beside each vendor's
// CredentialKeys and this holds the two together.
func TestRecognisedKeysAreTheVendorsCredentialKeys(t *testing.T) {
	t.Parallel()
	vendors := map[string][]string{
		"datadog": datadog.CredentialKeys,
		"gitlab":  gitlab.CredentialKeys,
		"github":  github.CredentialKeys,
	}
	for _, b := range config.SeatToolBindings() {
		keys, ok := vendors[b.Vendor]
		if !ok {
			continue
		}
		for _, k := range b.Keys {
			for _, key := range []string{k.Env, k.Header} {
				if key != "" && !slices.Contains(keys, key) {
					t.Errorf("%s's recognised server reads %q, which %s.CredentialKeys %q lacks",
						b.Vendor, key, b.Vendor, keys)
				}
			}
		}
	}
}
