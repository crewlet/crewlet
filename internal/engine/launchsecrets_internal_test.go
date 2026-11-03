package engine

import (
	"maps"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A LAUNCH'S SECRETS ARE ITS CREDENTIAL POSITIONS, AND ONLY THOSE: what each
// reference in the run's environment resolved to — the whole value's, or a
// part's inside a setting, whatever its variable is named — a variable whose
// name says it carries a credential, written in the document as much as
// referenced, the entry's sign-in beneath the seat's, and a setup file that was
// one reference whole. Not the seat's handle or email, a setting written in the
// document, or a setup step's own script: none is a secret, and each, scrubbed
// by value, was lost from every kept transcript that named it — the seat's own
// name read as a redacted secret in every commit it made.
//
// Mutation: hand the sandbox the whole environment again, drop a reference's
// value, stop reading a credential's name, take every setup file whatever its
// body, or count the engine's identity facts among the references, and a check
// below goes red.
func TestALaunchsSecretsAreItsCredentialPositionsAndOnlyThose(t *testing.T) {
	t.Setenv(config.HarnessHomeEnv, t.TempDir())
	t.Setenv("ENTRY_KEY", "sk-ant-company-entry-key")
	t.Setenv("PROXY_PASS", "the-proxys-own-password")
	t.Setenv("DEPLOY_KEY", "the-deploy-keys-own-body")
	t.Setenv("SEAT_EMAIL", "agent-engineer@example.com")
	c := companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    claude:
      model: claude-sonnet-5
      auth: {mode: api-key, api_key: "${ENTRY_KEY}"}
roles:
  - name: Engineer
    handle: agent-engineer
    email: "${SEAT_EMAIL}"
    llm: claude
`)
	seat := seatNamed(t, c, "Engineer")
	e := &Engine{}
	steps := []sandbox.SetupStep{{Name: "keys", Files: map[string]string{
		".ssh/id_ed25519": "${DEPLOY_KEY}",
		"setup.sh":        "git config user.name the-seats-own-script-line",
	}}}
	run := e.runEnvironment(t.Context(), nil, seat, &org.RoleSandbox{Env: map[string]string{
		"HTTPS_PROXY": "http://agent:${PROXY_PASS}@proxy.example.com",
		"NPM_TOKEN":   "npm-token-written-in-the-document",
		"NODE_ENV":    "production-settings",
	}}, steps)
	member, err := c.Models.Head(seat, phase.Execute)
	if err != nil {
		t.Fatalf("the executor chain: %v", err)
	}
	agent := member.Provider.(*harness.Provider)
	executor := newExecutorRun(agent, run.env)
	secrets := launchSecrets(agent, run, executor.env)

	for class, want := range map[string]string{
		"a reference inside a setting":        "the-proxys-own-password",
		"a credential's name, written":        "npm-token-written-in-the-document",
		"the entry's sign-in":                 "sk-ant-company-entry-key",
		"a setup file that was one reference": "the-deploy-keys-own-body",
	} {
		if !slices.Contains(secrets, want) {
			t.Errorf("%s (%q) is not among the launch's secrets: %q", class, want, secrets)
		}
	}
	for class, not := range map[string]string{
		"the seat's handle":                 "agent-engineer",
		"the seat's email, a fact":          "agent-engineer@example.com",
		"a setting written in the document": "production-settings",
		"the setting a reference is inside": "http://agent:the-proxys-own-password@proxy.example.com",
		"a setup step's own script":         "git config user.name the-seats-own-script-line",
	} {
		if slices.Contains(secrets, not) {
			t.Errorf("%s (%q) is among the launch's secrets, and is no secret: %q", class, not, secrets)
		}
	}
}

// AN MCP SERVER'S URL HOLDS WHAT ITS REFERENCES RESOLVED TO, AND THE BOX'S SCRUB
// IS TOLD SO: a secret an operator keeps in a URL's path rather than a header is
// in the box's MCP configuration as surely as a header is, and with none of
// redact's shapes it survived every capture.
//
// Mutation: leave the url's references off the server the box is handed, or
// render a server's url secret into a stdio server's fields, and a check below
// goes red.
//
// BOTH SERVERS ALLOW EVERY TOOL, so both reach the box: a server holding a tool
// back at Needs approval is kept out of it and reached over the bridge instead
// ([sandboxMCP]), and a case whose servers were kept out read an empty
// configuration — the stdio check below passing only because its server was not
// there to read. So the case first asserts both are in the box.
func TestAnMCPServersURLSecretIsHandedToTheScrub(t *testing.T) {
	t.Parallel()
	company := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: zapier
    transport: http
    shared: false
    url: "https://mcp.example.com/api/mcp/s/${ZAPIER_MCP_SECRET}/mcp"
    tools: {default: allow}
  - name: search
    command: "search-server"
    env:
      SEARCH_KEY: "${SEARCH_KEY}"
    tools: {default: allow}
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`)
	seat := company.Org.AgentSeatByHandle("ceo")
	env := config.NewResolver(config.MapSource{
		"ZAPIER_MCP_SECRET": "zapier-path-secret-value", "SEARCH_KEY": "search-secret",
	})
	gate := &org.RoleSandbox{MCP: org.RoleSandboxMCP{Servers: []string{"zapier", "search"}}}

	got, withheld := sandboxMCP(env, company, seat, gate, nil)
	if len(withheld) != 0 {
		t.Fatalf("withheld = %+v: every reference resolves", withheld)
	}
	for _, server := range []string{"zapier", "search"} {
		if _, inBox := got[server]; !inBox {
			t.Fatalf("the box was handed %v, without %s: a server the box is not handed hands "+
				"the scrub nothing, and every check below would read nothing", slices.Sorted(maps.Keys(got)),
				server)
		}
	}
	if secrets := got["zapier"].Secrets; !slices.Equal(secrets, []string{"zapier-path-secret-value"}) {
		t.Errorf("the box's zapier server hands the scrub %q, want what its url's reference resolved to",
			secrets)
	}
	if secrets := got["search"].Secrets; len(secrets) != 0 {
		t.Errorf("a stdio server with no url hands the scrub %q from a url it has not got", secrets)
	}
}
