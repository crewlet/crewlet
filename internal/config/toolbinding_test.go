package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

func stdio(name, command string, args ...string) config.MCPServer {
	return config.MCPServer{Name: name, Transport: config.TransportStdio, Command: command, Args: args, Shared: org.Off()}
}

func remote(name, url string) config.MCPServer {
	return config.MCPServer{Name: name, Transport: config.TransportHTTP, URL: url, Shared: org.Off()}
}

// EVERY SERVER THIS BUILD RECOGNISES reads the seat credential its own
// documentation names, under the key its transport reads, whatever the entry
// is called.
func TestRecognisedServersReadTheirOwnKeys(t *testing.T) {
	t.Parallel()
	bound := func(kind, field, key, prefix string) config.BoundSeatKey {
		return config.BoundSeatKey{Kind: kind, Field: field, Key: key, Prefix: prefix}
	}
	for name, tc := range map[string]struct {
		server config.MCPServer
		want   []config.BoundSeatKey
	}{
		"Datadog's remote server": {remote("dd", "https://mcp.datadoghq.com/v1/mcp"),
			[]config.BoundSeatKey{bound("datadog", "APP_KEY", "DD_APPLICATION_KEY", "")}},
		"Datadog's remote server on another site": {remote("dd", "https://mcp.us5.datadoghq.com/v1/mcp"),
			[]config.BoundSeatKey{bound("datadog", "APP_KEY", "DD_APPLICATION_KEY", "")}},
		"Datadog's government site": {remote("dd", "https://mcp.ddog-gov.com/v1/mcp"),
			[]config.BoundSeatKey{bound("datadog", "APP_KEY", "DD_APPLICATION_KEY", "")}},
		"Datadog's EU site": {remote("dd", "https://mcp.datadoghq.eu/v1/mcp"),
			[]config.BoundSeatKey{bound("datadog", "APP_KEY", "DD_APPLICATION_KEY", "")}},
		// A SEAT'S KEY GOES ONLY TO A DATADOG SITE: a host that merely
		// contains a site's name is somebody else's server.
		"a host that ends in a site's name":  {remote("dd", "https://mcp.datadoghq.com.example.net/v1/mcp"), nil},
		"a host that contains a site's name": {remote("dd", "https://evil-datadoghq.com/v1/mcp"), nil},
		"a site's name as a subdomain":       {remote("dd", "https://datadoghq.evil.example/v1/mcp"), nil},
		"GitHub's remote server": {remote("gh", "https://api.githubcopilot.com/mcp/"),
			[]config.BoundSeatKey{bound("github", "APP_TOKEN", "Authorization", "Bearer ")}},
		"GitHub's local server": {stdio("gh", "docker", "run", "-i", "ghcr.io/github/github-mcp-server"),
			[]config.BoundSeatKey{bound("github", "APP_TOKEN", "GITHUB_PERSONAL_ACCESS_TOKEN", "")}},
		"glab": {stdio("gl", "glab", "mcp", "serve"),
			[]config.BoundSeatKey{bound("gitlab", "TOKEN", "GITLAB_TOKEN", "")}},
		"glab at a full path": {stdio("gl", "/usr/local/bin/glab", "mcp", "serve"),
			[]config.BoundSeatKey{bound("gitlab", "TOKEN", "GITLAB_TOKEN", "")}},
		"GitLab's community server": {stdio("gl", "npx", "-y", "@modelcontextprotocol/server-gitlab"),
			[]config.BoundSeatKey{bound("gitlab", "TOKEN", "GITLAB_PERSONAL_ACCESS_TOKEN", "")}},
		"slack-mcp-server": {stdio("chat", "npx", "-y", "slack-mcp-server@latest", "--transport", "stdio"),
			[]config.BoundSeatKey{bound("slack", "BOT_TOKEN", "SLACK_MCP_XOXB_TOKEN", "")}},
		"the Mattermost server": {stdio("mm", "mcp-server-mattermost"),
			[]config.BoundSeatKey{bound("mattermost", "BOT_TOKEN", "MATTERMOST_TOKEN", "")}},
		"mcp-atlassian, both fields for both products": {stdio("atl", "docker", "run", "-i", "ghcr.io/sooperset/mcp-atlassian:latest"),
			[]config.BoundSeatKey{
				bound("atlassian", "TOKEN", "JIRA_API_TOKEN", ""), bound("atlassian", "EMAIL", "JIRA_USERNAME", ""),
				bound("atlassian", "TOKEN", "CONFLUENCE_API_TOKEN", ""), bound("atlassian", "EMAIL", "CONFLUENCE_USERNAME", ""),
			}},
		"a server nobody recognises": {stdio("x", "my-server"), nil},
	} {
		if got := tc.server.SeatBindings(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: bindings %+v, want %+v", name, got, tc.want)
		}
	}
	shared := remote("dd", "https://mcp.datadoghq.com/v1/mcp")
	shared.Shared = org.On()
	if got := shared.SeatBindings(); got != nil {
		t.Errorf("a shared server reads a seat's credential: %+v", got)
	}
}

// A DECLARATION WINS OVER RECOGNITION: the entry says what it reads, and the
// default is not added beside it.
func TestADeclarationOverridesTheRecognisedDefault(t *testing.T) {
	t.Parallel()
	server := stdio("slack", "npx", "-y", "slack-mcp-server")
	server.SeatCredentials = []config.SeatCredential{{Kind: "slack", Key: "SLACK_TOKEN"}}
	want := []config.BoundSeatKey{{Kind: "slack", Field: "BOT_TOKEN", Key: "SLACK_TOKEN"}}
	if got := server.SeatBindings(); !reflect.DeepEqual(got, want) {
		t.Errorf("bindings %+v, want %+v", got, want)
	}
	// AND AN UNRECOGNISED SERVER IS BOUND BY ITS DECLARATION ALONE.
	custom := remote("tracker", "https://mcp.example.com/")
	custom.SeatCredentials = []config.SeatCredential{
		{Kind: "atlassian", Field: "EMAIL", Key: "X-Atlassian-User"},
		{Kind: "atlassian", Key: "Authorization", Prefix: "Bearer "},
	}
	want = []config.BoundSeatKey{
		{Kind: "atlassian", Field: "EMAIL", Key: "X-Atlassian-User"},
		{Kind: "atlassian", Field: "TOKEN", Key: "Authorization", Prefix: "Bearer "},
	}
	if got := custom.SeatBindings(); !reflect.DeepEqual(got, want) {
		t.Errorf("bindings %+v, want %+v", got, want)
	}
}

// A DECLARATION IS CHECKED: the integration and field exist, the key is
// given, it agrees with the transport, and a shared server declares none.
func TestASeatCredentialDeclarationIsChecked(t *testing.T) {
	t.Parallel()
	doc := func(server string) string {
		return "name: Acme\nmcp_servers:\n" + server
	}
	for name, tc := range map[string]struct{ server, path string }{
		"an unknown integration": {"  - {name: x, command: x, shared: false, seat_credentials: [{kind: jira, key: K}]}\n",
			"mcp_servers[0].seat_credentials[0].kind"},
		"a field the integration has not": {"  - {name: x, command: x, shared: false, seat_credentials: [{kind: slack, field: EMAIL, key: K}]}\n",
			"mcp_servers[0].seat_credentials[0].field"},
		"no key": {"  - {name: x, command: x, shared: false, seat_credentials: [{kind: slack}]}\n",
			"mcp_servers[0].seat_credentials[0].key"},
		"a header on a stdio server": {"  - {name: x, command: x, shared: false, seat_credentials: [{kind: slack, key: K, as: header}]}\n",
			"mcp_servers[0].seat_credentials[0].as"},
		"a shared server": {"  - {name: x, command: x, seat_credentials: [{kind: slack, key: K}]}\n",
			"mcp_servers[0].seat_credentials"},
	} {
		_, err := config.ParseCompany([]byte(doc(tc.server)))
		if err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: answered %v, want a refusal at %s", name, err, tc.path)
		}
	}
	ok := "  - {name: x, transport: http, url: \"https://mcp.example.com/\", shared: false, " +
		"seat_credentials: [{kind: gitlab, key: Private-Token, as: header}]}\n"
	if _, err := config.ParseCompany([]byte(doc(ok))); err != nil {
		t.Errorf("a well-formed declaration was refused: %v", err)
	}
}

// THE BINDING LANDS WHERE THE ORG MODEL IS BUILT: a seat that is on at an
// integration gets its own sealed credential under the key the server reads,
// over a value written for the same key, and a seat that is off gets nothing.
func TestTheSeatsCredentialIsBoundToEveryServerThatReadsIt(t *testing.T) {
	t.Parallel()
	on, off := true, false
	headers := remote("tracker", "https://mcp.example.com/")
	headers.SeatCredentials = []config.SeatCredential{{Kind: "slack", Key: "Authorization", Prefix: "Bearer "}}
	cfg := &config.Company{
		Name: "acme",
		// THE ENGINE HOLDS SLACK'S CREDENTIALS here (D4b), so the binding is
		// the name it seals them under.
		Integrations: config.Integrations{Slack: &config.Slack{AppConfigToken: "${SLACK_APP_CONFIG_TOKEN}"}},
		MCPServers:   []config.MCPServer{stdio("chat", "npx", "-y", "slack-mcp-server"), headers},
		Roles: []config.Role{
			{Name: "Engineer", Handle: "swe", MCPEnv: map[string]map[string]string{
				"chat":    {"SLACK_MCP_XOXB_TOKEN": "${SOMETHING_ELSE}", "SLACK_MCP_ADD_MESSAGE_TOOL": "true"},
				"tracker": {"authorization": "Bearer ${STALE}"},
			}, Integrations: config.RoleIntegrations{Slack: &config.RoleSlack{Enabled: &on}}},
			{Name: "Designer", Integrations: config.RoleIntegrations{Slack: &config.RoleSlack{Enabled: &off}}},
		},
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	swe := o.AgentSeatByHandle("swe")
	if got := swe.MCPEnv["chat"]; got["SLACK_MCP_XOXB_TOKEN"] != "${SLACK_BOT_TOKEN_SWE}" || got["SLACK_MCP_ADD_MESSAGE_TOOL"] != "true" {
		t.Errorf("the recognised server reads %v", got)
	}
	tracker := swe.MCPEnv["tracker"]
	if len(tracker) != 1 || tracker["Authorization"] != "Bearer ${SLACK_BOT_TOKEN_SWE}" {
		t.Errorf("the declared header reads %v, want the binding in place of the unit's value in any case", tracker)
	}
	// THE DERIVED HANDLE: Designer declares none and is off.
	if designer := o.AgentSeatByHandle("designer"); designer == nil || len(designer.MCPEnv["chat"]) != 0 {
		t.Errorf("a seat that is off was bound: %+v", designer)
	}
}

// A PER-SEAT SERVER NAMED AFTER AN INTEGRATION THAT READS NOTHING is reported;
// one it recognises, one that declares, and a shared one are not.
func TestUnboundSeatServersAreTheOnesThatActAsNobody(t *testing.T) {
	t.Parallel()
	declared := stdio("datadog", "my-dd")
	declared.SeatCredentials = []config.SeatCredential{{Kind: "datadog", Key: "DD_KEY"}}
	shared := stdio("jira", "my-jira")
	shared.Shared = org.On()
	cfg := &config.Company{MCPServers: []config.MCPServer{
		stdio("gitlab", "my-gitlab"), stdio("confluence", "my-wiki"), declared, shared,
		stdio("slack", "npx", "slack-mcp-server"),
	}}
	for kind, want := range map[string][]string{
		"gitlab": {"gitlab"}, "atlassian": {"confluence"}, "datadog": nil, "slack": nil,
	} {
		if got := cfg.UnboundSeatServers(kind); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: unbound %v, want %v", kind, got, want)
		}
	}
}

// EXTERNAL CREDENTIALS STAY WHERE THE SEAT NAMES THEM (D4b): a company that
// does not provision an integration hands its servers the seat's own
// position (a Mattermost bot token), and a server reading a credential the
// seat names only in its mcp_env keeps that value; the engine binds no name it
// never seals.
func TestAnExternalCredentialIsBoundFromWhereTheSeatNamesIt(t *testing.T) {
	t.Parallel()
	on := true
	cfg := &config.Company{
		Name: "acme",
		Integrations: config.Integrations{
			Mattermost: &config.Mattermost{Enabled: true},
			Datadog:    &config.Datadog{},
		},
		MCPServers: []config.MCPServer{
			stdio("mm", "mcp-server-mattermost"),
			remote("dd", "https://mcp.datadoghq.com/v1/mcp"),
		},
		Roles: []config.Role{{
			Name: "SRE", Handle: "sre",
			MCPEnv: map[string]map[string]string{"dd": {"DD_APPLICATION_KEY": "${SRE_OWN_DD_KEY}"}},
			Integrations: config.RoleIntegrations{
				Mattermost: &config.RoleMattermost{Enabled: &on, BotToken: "${SRE_MM_TOKEN}"},
				Datadog:    &config.RoleDatadog{Enabled: &on},
			},
		}},
	}
	if mode := cfg.SeatCredentialMode("mattermost", false); mode != config.SeatCredentialExternal {
		t.Fatalf("the fixture's Mattermost is %q, want external", mode)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	sre := o.AgentSeatByHandle("sre")
	if got := sre.MCPEnv["mm"]["MATTERMOST_TOKEN"]; got != "${SRE_MM_TOKEN}" {
		t.Errorf("the Mattermost server reads %q, want the seat's own bot token", got)
	}
	if got := sre.MCPEnv["dd"]["DD_APPLICATION_KEY"]; got != "${SRE_OWN_DD_KEY}" {
		t.Errorf("the Datadog server reads %q, want the seat's own value left alone", got)
	}
}
