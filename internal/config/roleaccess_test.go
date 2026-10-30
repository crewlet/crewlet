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
// the key the server reads, and only for a seat that is on. Both integrations
// are ones the engine provisions, so the credential is the one it seals (D4b).
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

// THE ENGINE-WRITTEN FIELDS are reported when a write changes them: a GitHub
// App's record on a seat, matched by its derived handle wherever it moved; one
// on a seat new to the document; and the migration's marker. An unchanged
// record, and the marker in another order, are not changes.
func TestEngineWrittenChanges(t *testing.T) {
	t.Parallel()
	prior := parsedDoc(t, `
name: Acme
seat_access_migrated: [github, slack]
roles:
  - {name: SRE, handle: sre, integrations: {github: {enabled: true, app_id: 7, app_slug: acme-sre, installation_id: 9}}}
  - {name: Planner, integrations: {github: {enabled: true, app_id: 5, app_slug: acme-planner}}}
`)
	same := parsedDoc(t, `
name: Acme
seat_access_migrated: [slack, github]
units:
  - name: Ops
    roles:
      - {name: SRE, handle: sre, integrations: {github: {enabled: false, app_id: 7, app_slug: acme-sre, installation_id: 9}}}
roles:
  - {name: Planner, integrations: {github: {enabled: true, app_id: 5, app_slug: acme-planner}}}
`)
	if got := EngineWrittenChanges(prior, same); len(got) != 0 {
		t.Fatalf("a write that moved and switched seats but kept their records changed %v", got)
	}
	next := parsedDoc(t, `
name: Acme
seat_access_migrated: [github]
roles:
  - {name: SRE, handle: sre, integrations: {github: {enabled: true, app_id: 8, app_slug: acme-sre, installation_id: 9}}}
  - {name: Planner, integrations: {github: {enabled: true, app_id: 5, app_slug: acme-planner}}}
  - {name: CTO, handle: cto, integrations: {github: {enabled: true, app_id: 3}}}
`)
	got := EngineWrittenChanges(prior, next)
	want := []string{"seat_access_migrated", "roles[0].integrations.github.app_id",
		"roles[2].integrations.github.app_id"}
	if !slices.Equal(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
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

// PRODUCT ACCESS ONLY: a Jira or Confluence seat that is on with no role and
// no list is a setting of its own; a list without a role is refused, because
// the list names where a role is held.
func TestProductAccessOnlyIsASettingAndAListNeedsARole(t *testing.T) {
	t.Parallel()
	const connected = `
name: Acme
integrations:
  atlassian: {org_id: org-1, api_key: "${ATLASSIAN_KEY}"}
  jira: {url: "https://acme.atlassian.net", email: admin@acme.example, token: "${JIRA_TOKEN}", webhook_secret: "${JIRA_HOOK}"}
  confluence: {url: "https://acme.atlassian.net/wiki", email: admin@acme.example, token: "${CONF_TOKEN}"}
units:
  - name: Ops
    id: ops
    project: OPS
    space: ENG
    roles:
`
	if _, err := ParseCompany([]byte(connected +
		"      - {name: SRE, handle: sre, integrations: {jira: {enabled: true}, confluence: {enabled: true}}}\n")); err != nil {
		t.Fatalf("product access only was refused: %v", err)
	}
	for _, tc := range []struct{ seat, path string }{
		{"{jira: {enabled: true, projects: [OPS]}}", "units[0].roles[0].integrations.jira.project_role"},
		{"{confluence: {enabled: true, spaces: [ENG]}}", "units[0].roles[0].integrations.confluence.space_role"},
	} {
		err := rejects(t, connected+"      - {name: SRE, handle: sre, integrations: "+tc.seat+"}\n", tc.path)
		if !errors.Is(err, ErrMissing) {
			t.Fatalf("%s: want ErrMissing, got %v", tc.path, err)
		}
	}
}

// A KEY IS WITHIN REACH IN ANY CASE ITS VENDOR IGNORES: a seat's lower-case
// Jira project or Confluence space key is the upper-case one a unit names.
func TestAKeyIsWithinReachInAnyCaseItsVendorIgnores(t *testing.T) {
	t.Parallel()
	const company = `
name: Acme
integrations:
  atlassian: {org_id: org-1, api_key: "${ATLASSIAN_KEY}"}
  jira: {url: "https://acme.atlassian.net", email: admin@acme.example, token: "${JIRA_TOKEN}", webhook_secret: "${JIRA_HOOK}"}
  confluence: {url: "https://acme.atlassian.net/wiki", email: admin@acme.example, token: "${CONF_TOKEN}"}
units:
  - name: Ops
    id: ops
    project: OPS
    space: ENG
    roles:
`
	if _, err := ParseCompany([]byte(company + "      - {name: SRE, handle: sre, integrations: " +
		"{jira: {enabled: true, project_role: \"10101\", projects: [ops]}, " +
		"confluence: {enabled: true, space_role: r-1, spaces: [eng]}}}\n")); err != nil {
		t.Fatalf("keys in another case were refused: %v", err)
	}
	err := rejects(t, company+"      - {name: SRE, handle: sre, integrations: "+
		"{jira: {enabled: true, project_role: \"10101\", projects: [web]}}}\n",
		"units[0].roles[0].integrations.jira.projects[0]")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a project outside reach: want ErrConflict, got %v", err)
	}
}
