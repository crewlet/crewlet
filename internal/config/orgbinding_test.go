package config_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
)

// orgCompany is a company whose Datadog and Mattermost integrations hold their
// own values, with one seat on at both and one off.
func orgCompany(servers ...config.MCPServer) *config.Company {
	on, off := true, false
	return &config.Company{
		Name: "acme",
		Integrations: config.Integrations{
			Datadog: &config.Datadog{Enabled: true, Provisioning: &config.DatadogProvisioning{
				Site: "datadoghq.com", APIKey: "${DATADOG_API_KEY}", AppKey: "${DATADOG_APP_KEY}"}},
			Mattermost: &config.Mattermost{Enabled: true, URL: "https://chat.example.com", Team: "acme"},
		},
		MCPServers: servers,
		Roles: []config.Role{
			{Name: "SRE", Handle: "sre", Integrations: config.RoleIntegrations{
				Datadog:    &config.RoleDatadog{Enabled: &on},
				Mattermost: &config.RoleMattermost{Enabled: &on}}},
			{Name: "Designer", Handle: "designer", Integrations: config.RoleIntegrations{
				Datadog: &config.RoleDatadog{Enabled: &off}}},
		},
	}
}

// THE INTEGRATION HANDS EVERY SEAT ITS OWN VALUES BESIDE THE SEAT'S KEY, and a
// seat that is off neither. Mutation: drop the org keys from bindSeatTools, and
// the seat's server is started without DD_API_KEY.
func TestTheIntegrationHandsEverySeatItsOwnValuesBesideTheSeatsKey(t *testing.T) {
	t.Parallel()
	cfg := orgCompany(remote("datadog", "https://mcp.datadoghq.com/v1/mcp"),
		stdio("mattermost", "uvx", "mcp-server-mattermost==0.6.1"))
	o, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	sre := o.AgentSeatByHandle("sre")
	dd := sre.MCPEnv["datadog"]
	if len(dd) != 2 || dd["DD_API_KEY"] != "${DATADOG_API_KEY}" ||
		dd["DD_APPLICATION_KEY"] != "${"+envref.SealedName("datadog", "APP_KEY", "sre")+"}" {
		t.Errorf("Datadog's server is handed %v, want the organization's key beside the seat's own", dd)
	}
	if got := sre.MCPEnv["mattermost"]["MATTERMOST_URL"]; got != "https://chat.example.com" {
		t.Errorf("Mattermost's server is handed the address %q, want the integration's", got)
	}
	if designer := o.AgentSeatByHandle("designer"); len(designer.MCPEnv["datadog"]) != 0 {
		t.Errorf("a seat that is off was handed %v", designer.MCPEnv["datadog"])
	}
}

// A VALUE THE ENTRY WRITES WINS over the integration's, in whatever case it
// spells the header, and so does one the seat's block writes: the
// integration's value fills only a key nobody wrote.
func TestAnExplicitValueWinsOverTheIntegrations(t *testing.T) {
	t.Parallel()
	server := remote("datadog", "https://mcp.datadoghq.com/v1/mcp")
	server.Headers = map[string]string{"dd_api_key": "${OTHER_ORG_KEY}"}
	cfg := orgCompany(server)
	o, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	if got := o.AgentSeatByHandle("sre").MCPEnv["datadog"]; len(got) != 1 || got["DD_APPLICATION_KEY"] == "" {
		t.Errorf("the seat's block is %v, want only its own key beside the entry's header", got)
	}
	if handed := cfg.HandedOrgKeys(&cfg.MCPServers[0]); len(handed) != 0 {
		t.Errorf("the integration hands %v over a header the entry writes", handed)
	}

	seat := orgCompany(remote("datadog", "https://mcp.datadoghq.com/v1/mcp"))
	seat.Roles[0].MCPEnv = map[string]map[string]string{"datadog": {"DD_API_KEY": "seat-own"}}
	o, err = seat.Organization()
	if err != nil {
		t.Fatal(err)
	}
	if got := o.AgentSeatByHandle("sre").MCPEnv["datadog"]["DD_API_KEY"]; got != "seat-own" {
		t.Errorf("the seat's own DD_API_KEY became %q", got)
	}
}

// THE SERVER'S VALUES SAY THE INTEGRATION HANDS THE KEY, with the variable it
// reads, so an edit keeps it as the integration's rather than as a stored
// header; a header the entry writes is the entry's own.
func TestTheServersValuesNameTheIntegrationsKey(t *testing.T) {
	t.Parallel()
	cfg := orgCompany(remote("datadog", "https://mcp.datadoghq.com/v1/mcp"))
	keys, ok := cfg.MCPValueKeys("datadog", "")
	if !ok {
		t.Fatal("no values for a declared server")
	}
	byKey := map[string]config.MCPValueKey{}
	for _, k := range keys {
		byKey[k.Key] = k
	}
	if k := byKey["DD_API_KEY"]; k.BoundBy != "datadog" || k.Variable != "DATADOG_API_KEY" {
		t.Errorf("DD_API_KEY reads %+v, want the Datadog integration's DATADOG_API_KEY", k)
	}
	if k := byKey["DD_APPLICATION_KEY"]; k.BoundBy != "datadog" || k.Variable != "" {
		t.Errorf("DD_APPLICATION_KEY reads %+v, want each agent's own at the server's level", k)
	}

	written := remote("datadog", "https://mcp.datadoghq.com/v1/mcp")
	written.Headers = map[string]string{"DD_API_KEY": "${OTHER_ORG_KEY}"}
	keys, _ = orgCompany(written).MCPValueKeys("datadog", "")
	for _, k := range keys {
		if k.Key == "DD_API_KEY" && (k.BoundBy != "" || k.Variable != "OTHER_ORG_KEY") {
			t.Errorf("the entry's own header reads %+v", k)
		}
	}
}
