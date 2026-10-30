package config

import (
	"errors"
	"strings"
	"testing"
)

// slackServer is a per-seat Slack tool server, so a seat's mcp_env.slack names
// a server the document declares.
const slackServer = `
mcp_servers:
  - {name: slack, command: slack-mcp-server, shared: false}
  - {name: weather, command: weather-mcp, shared: false}
`

// EVERY SEAT BLOCK SAYS WHETHER THE AGENT IS ON (D9: off until turned on).
func TestASeatBlockWithoutEnabledIsRefused(t *testing.T) {
	t.Parallel()
	err := rejects(t, `
name: Acme
roles:
  - {name: SRE, handle: sre, integrations: {datadog: {role: r-1}}}
`, "roles[0].integrations.datadog.enabled")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
}

// D4b, BOTH MODES: where the engine provisions the integration, a seat names
// no credential for it in mcp_env; where it does not, the seat's own variable
// is how the credential arrives.
func TestAnMCPEnvCredentialFollowsTheIntegrationsMode(t *testing.T) {
	t.Parallel()
	seat := `
roles:
  - {name: SRE, handle: sre, mcp_env: {slack: {SLACK_MCP_XOXB_TOKEN: "${SRE_SLACK}"}}}
`
	err := rejects(t, "name: Acme\nintegrations:\n  slack: {}\n"+slackServer+seat,
		"roles[0].mcp_env.slack.SLACK_MCP_XOXB_TOKEN")
	if !strings.Contains(err.Error(), "provisions slack") {
		t.Fatalf("the refusal must say the engine provisions slack: %v", err)
	}
	if _, err := ParseCompany([]byte("name: Acme\n" + slackServer + seat)); err != nil {
		t.Fatalf("an external Slack credential in mcp_env was refused: %v", err)
	}
	// A UNIT'S BLOCK IS EVERY MEMBER'S, so in engine mode it names none.
	rejects(t, "name: Acme\nintegrations:\n  slack: {}\n"+slackServer+`
units:
  - name: Ops
    id: ops
    mcp_env: {slack: {SLACK_MCP_XOXB_TOKEN: "${OPS_SLACK}"}}
    roles: [{name: SRE, handle: sre}]
`, "units[0].mcp_env.slack.SLACK_MCP_XOXB_TOKEN")
}

// D4a, BOTH MODES: a role credential position names only the seat's own sealed
// name where the engine provisions, and never anybody's sealed name where it
// does not.
func TestARoleCredentialPositionFollowsTheIntegrationsMode(t *testing.T) {
	t.Parallel()
	engine := "name: Acme\nintegrations:\n  slack: {}\nroles:\n  - name: SRE\n    handle: sre\n" +
		"    integrations: {slack: {enabled: true, bot_token: %q}}\n"
	rejects(t, strings.Replace(engine, "%q", `"${SOME_TOKEN}"`, 1), "roles[0].integrations.slack.bot_token")
	if _, err := ParseCompany([]byte(strings.Replace(engine, "%q", `"${SLACK_BOT_TOKEN_SRE}"`, 1))); err != nil {
		t.Fatalf("the seat's own sealed name was refused in engine mode: %v", err)
	}
	external := `
name: Acme
roles:
  - {name: CTO, handle: cto}
  - name: SRE
    handle: sre
    integrations: {slack: {enabled: false, bot_token: "${SLACK_BOT_TOKEN_CTO}"}}
`
	err := rejects(t, external, "roles[1].integrations.slack.bot_token")
	if !strings.Contains(err.Error(), "engine's own name") {
		t.Fatalf("the refusal must say the name is the engine's: %v", err)
	}
	if _, err := ParseCompany([]byte(strings.Replace(external, "${SLACK_BOT_TOKEN_CTO}", "${SRE_OWN_SLACK}", 1))); err != nil {
		t.Fatalf("an exclusive external variable was refused: %v", err)
	}
}

// A SEAT'S CREDENTIAL HAS ONE READER: shared with another seat, refused.
func TestASeatCredentialSharedWithAnotherSeatIsRefused(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\n"+slackServer+`
roles:
  - {name: SRE, handle: sre, mcp_env: {weather: {KEY: "${WEATHER_KEY}"}}}
  - {name: CTO, handle: cto, mcp_env: {weather: {KEY: "${WEATHER_KEY}"}}}
`, "roles[1]")
	if !strings.Contains(err.Error(), "also named by seat sre") {
		t.Fatalf("the refusal must name the other seat: %v", err)
	}
}

// ANYWHERE IN A SEAT: its own sealed names are readable; another seat's, or a
// company credential, are not.
func TestASeatReadsOnlyItsOwnSealedNames(t *testing.T) {
	t.Parallel()
	doc := `
name: Acme
integrations:
  github: {enabled: false, webhook_secret: "${GITHUB_HOOK}"}
roles:
  - {name: CTO, handle: cto}
  - name: SRE
    handle: sre
    sandbox: {env: {GITLAB_TOKEN: %q}}
`
	if _, err := ParseCompany([]byte(strings.Replace(doc, "%q", `"${GITLAB_TOKEN_SRE}"`, 1))); err != nil {
		t.Fatalf("a seat's own sealed name was refused in its sandbox: %v", err)
	}
	err := rejects(t, strings.Replace(doc, "%q", `"${GITLAB_TOKEN_CTO}"`, 1), "roles[1]")
	if !strings.Contains(err.Error(), "seat cto's own credential") {
		t.Fatalf("the refusal must name the seat whose credential it is: %v", err)
	}
	err = rejects(t, strings.Replace(doc, "%q", `"${GITHUB_HOOK}"`, 1), "roles[1]")
	if !strings.Contains(err.Error(), "company integration's credential") {
		t.Fatalf("the refusal must say it is the company's: %v", err)
	}
}

// THE ENGINE BINDS A SEAT'S IDENTITY TO ITS OWN SEALED NAME where it
// provisions, once the integration is migrated, and a seat that is off holds
// no identity; before the migration the identity is the document's as written.
func TestTheOrgModelBindsASeatsIdentityByMode(t *testing.T) {
	t.Parallel()
	doc := `
name: Acme
seat_access_migrated: [slack]
integrations:
  slack: {}
roles:
  - {name: SRE, handle: sre, integrations: {slack: {enabled: true}}}
  - {name: CTO, handle: cto, integrations: {slack: {enabled: false}}}
`
	c := parsedDoc(t, doc)
	o, err := c.Organization()
	if err != nil {
		t.Fatalf("Organization: %v", err)
	}
	sre := o.AgentSeatByHandle("sre")
	if sre.Slack.BotToken != "${SLACK_BOT_TOKEN_SRE}" || sre.Slack.SigningSecret != "${SLACK_SIGNING_SECRET_SRE}" {
		t.Errorf("sre's Slack identity = %+v, want its own sealed names", sre.Slack)
	}
	if cto := o.AgentSeatByHandle("cto"); cto.Slack.BotToken != "" {
		t.Errorf("an agent that is off holds a Slack identity: %+v", cto.Slack)
	}

	unmigrated := parsedDoc(t, `
name: Acme
integrations:
  slack: {}
roles:
  - {name: SRE, handle: sre, integrations: {slack: {enabled: false, bot_token: "${SRE_BOT}", signing_secret: "${SRE_SIGN}"}}}
`)
	o, err = unmigrated.Organization()
	if err != nil {
		t.Fatalf("Organization: %v", err)
	}
	if got := o.AgentSeatByHandle("sre").Slack.BotToken; got != "${SRE_BOT}" {
		t.Errorf("before the migration the identity = %q, want the document's as written", got)
	}
}
