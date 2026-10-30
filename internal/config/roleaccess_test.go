package config

import (
	"errors"
	"slices"
	"testing"
)

// THE PER-SEAT BLOCKS' OWN RULES, refused on submit and placed where written.
func TestASeatAccessBlockIsHeldToItsVendorsTerms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, doc, path string
		kind            error
	}{
		{"a seat on at Mattermost with no server to post through", `
name: Acme
roles:
  - {name: SRE, handle: sre, integrations: {mattermost: {enabled: true, roles: [system_user]}}}
`, "roles[0].integrations.mattermost.enabled", ErrConflict},
		{"a seat on at Jira with no site", `
name: Acme
roles:
  - {name: SRE, handle: sre, integrations: {jira: {enabled: true, project_role: "10101"}}}
`, "roles[0].integrations.jira.enabled", ErrConflict},
		{"a GitLab role GitLab does not have", `
name: Acme
roles:
  - {name: SRE, handle: sre, integrations: {gitlab: {enabled: false, access_level: 35}}}
`, "roles[0].integrations.gitlab.access_level", ErrUnknownValue},
		{"a default and a custom GitLab role at once", `
name: Acme
roles:
  - {name: SRE, handle: sre, integrations: {gitlab: {enabled: false, access_level: 30, member_role_id: 4}}}
`, "roles[0].integrations.gitlab.member_role_id", ErrConflict},
		{"Mattermost roles it does not offer a bot", `
name: Acme
roles:
  - {name: SRE, handle: sre, integrations: {mattermost: {enabled: false, roles: [system_admin]}}}
`, "roles[0].integrations.mattermost.roles", ErrUnknownValue},
		{"a GitHub permission an agent's app does not hold", `
name: Acme
roles:
  - {name: SRE, handle: sre, integrations: {github: {enabled: false, permissions: {administration: write}}}}
`, "roles[0].integrations.github.permissions.administration", ErrUnknownValue},
		{"Workflows read-only, which GitHub does not grant", `
name: Acme
roles:
  - {name: SRE, handle: sre, integrations: {github: {enabled: false, permissions: {workflows: read}}}}
`, "roles[0].integrations.github.permissions.workflows", ErrUnknownValue},
		{"a Confluence permission not written operation:target", `
name: Acme
roles:
  - {name: SRE, handle: sre, integrations: {confluence: {enabled: false, operations: [read]}}}
`, "roles[0].integrations.confluence.operations[0]", ErrUnknownValue},
		{"a Jira project the company does not reach", `
name: Acme
units:
  - name: Ops
    id: ops
    project: OPS
    roles:
      - {name: SRE, handle: sre, integrations: {jira: {enabled: false, projects: [GOP]}}}
`, "units[0].roles[0].integrations.jira.projects[0]", ErrConflict},
		{"a block on a human seat", `
name: Acme
roles:
  - {name: Founder, handle: founder, kind: human, integrations: {datadog: {enabled: false}}}
`, "roles[0].integrations.datadog", ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, tc.doc, tc.path)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
		})
	}
}

// A COMPANY VARIABLE IN A SERVER'S OWN HEADERS IS ALLOWED, and a literal
// setting in a managed block is too.
func TestACompanyVariableInAServerHeaderIsAccepted(t *testing.T) {
	t.Parallel()
	_, err := ParseCompany([]byte(`
name: Acme
mcp_servers:
  - {name: datadog, transport: http, url: "https://mcp.datadoghq.com/v1/mcp", shared: false, headers: {DD_API_KEY: "${DATADOG_API_KEY}"}}
roles:
  - {name: SRE, handle: sre, mcp_env: {datadog: {DD_SITE: datadoghq.eu}}}
`))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
}

// THE ENGINE BINDS A SEAT'S OWN CREDENTIAL TO ITS PER-SEAT SERVER (D15), under
// the key the server reads, and only for a seat that is on.
func TestTheOrgModelBindsASeatsCredentialToItsTool(t *testing.T) {
	t.Parallel()
	c := parsedDoc(t, `
name: Acme
integrations:
  datadog: {enabled: true, provisioning: {site: datadoghq.com}}
  gitlab: {enabled: true, url: "https://gitlab.example.com", provisioning: {group: acme}}
mcp_servers:
  - {name: datadog, transport: http, url: "https://mcp.datadoghq.com/v1/mcp", shared: false}
  - {name: gitlab, command: gitlab-mcp, shared: false}
roles:
  - name: SRE
    handle: sre
    mcp_env: {datadog: {dd_application_key: "stale"}}
    integrations:
      datadog: {enabled: true, role: r-1}
      gitlab: {enabled: true, access_level: 30}
  - name: CTO
    handle: cto
    integrations:
      datadog: {enabled: false, role: r-1}
`)
	o, err := c.Organization()
	if err != nil {
		t.Fatalf("Organization: %v", err)
	}
	sre := o.AgentSeatByHandle("sre")
	if got := sre.MCPEnv["datadog"]; len(got) != 1 || got["DD_APPLICATION_KEY"] != "${DATADOG_APP_KEY_SRE}" {
		t.Errorf("sre's datadog block = %v, want only DD_APPLICATION_KEY bound to its own key", got)
	}
	if got := sre.MCPEnv["gitlab"]["GITLAB_PERSONAL_ACCESS_TOKEN"]; got != "${GITLAB_TOKEN_SRE}" {
		t.Errorf("sre's gitlab variable = %q", got)
	}
	if _, bound := o.AgentSeatByHandle("cto").MCPEnv["datadog"]; bound {
		t.Error("a seat that is off was bound a tool")
	}
}

// THE REFERENCES ARE COUNTED BY WHO NAMES THEM.
func TestReferencesAreCountedByWhoNamesThem(t *testing.T) {
	t.Parallel()
	c := parsedDoc(t, `
name: Acme
integrations:
  github: {enabled: false, webhook_secret: "${HOOK}"}
mcp_servers:
  - {name: weather, command: weather-mcp, env: {KEY: "${ORG_KEY}"}}
roles:
  - {name: SRE, handle: sre, mcp_env: {weather: {TOKEN: "${SRE_TOKEN}"}}}
`)
	refs, err := c.References()
	if err != nil {
		t.Fatal(err)
	}
	if refs.Integrations["HOOK"] != 1 || refs.SeatCredentials["sre"]["SRE_TOKEN"] != 1 {
		t.Fatalf("refs = %+v", refs)
	}
	if refs.OutsideSeat("SRE_TOKEN", "sre") != 0 || refs.OutsideSeat("ORG_KEY", "sre") != 1 {
		t.Fatalf("outside: SRE_TOKEN %d, ORG_KEY %d", refs.OutsideSeat("SRE_TOKEN", "sre"),
			refs.OutsideSeat("ORG_KEY", "sre"))
	}
}

// THE ENGINE-WRITTEN GITHUB FIELDS are reported when a write changes them.
func TestEngineWrittenChanges(t *testing.T) {
	t.Parallel()
	prior := parsedDoc(t, `
name: Acme
roles:
  - {name: SRE, handle: sre, integrations: {github: {enabled: true, app_id: 7, app_slug: acme-sre, installation_id: 9}}}
`)
	next := parsedDoc(t, `
name: Acme
roles:
  - {name: SRE, handle: sre, integrations: {github: {enabled: true, app_id: 8, app_slug: acme-sre, installation_id: 9}}}
  - {name: CTO, handle: cto, integrations: {github: {enabled: true, app_id: 3}}}
`)
	got := EngineWrittenChanges(prior, next)
	if !slices.Equal(got, []string{"roles[0].integrations.github.app_id"}) {
		t.Fatalf("changes = %v", got)
	}
}

func parsedDoc(t *testing.T, doc string) *Company {
	t.Helper()
	c, err := ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}
