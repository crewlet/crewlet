package webhooks_test

import (
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// Two configs, because one cannot hold every integration: Confluence and an
// enabled Plane are mutually exclusive — the knowledge backend is
// single-homed. That is a real constraint on a deployment, so the fixtures
// match it rather than working around it.

const selfHostedYAML = `
name: Acme
providers:
  llm:
    primary:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["key"]
integrations:
  forge_app_id: forge-app
  github:
    enabled: true
    webhook_secret: gh
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: gl
  plane:
    enabled: true
    url: https://plane.example.com
    workspace: acme
    webhook_secret: pl
roles:
  - name: CEO
    handle: ceo
    llm: primary
    integrations:
      slack:
        bot_token: xoxb-1
        signing_secret: ceo-signing
  - name: CTO
    handle: cto
    llm: primary
`

const atlassianYAML = `
name: Acme
providers:
  llm:
    primary:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["key"]
integrations:
  jira:
    url: https://acme.atlassian.net
    token: t
    webhook_secret: jira
  confluence:
    url: https://acme.atlassian.net/wiki
    token: t
    webhook_secret: conf
roles:
  - name: CEO
    handle: ceo
    llm: primary
`

func secretsFor(t *testing.T, yaml string) webhooks.Secrets {
	t.Helper()
	company, err := config.ParseCompany([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	organization, err := company.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	return webhooks.SecretsOf(company, organization)
}

func TestSecretsComeFromTheEpoch(t *testing.T) {
	t.Parallel()
	got := secretsFor(t, selfHostedYAML)
	atlassian := secretsFor(t, atlassianYAML)

	for _, tc := range []struct{ name, got, want string }{
		{"github", got.GitHub, "gh"},
		{"gitlab", got.GitLab, "gl"},
		{"plane", got.Plane, "pl"},
		{"forge app id", got.ForgeAppID, "forge-app"},
		{"jira", atlassian.Jira, "jira"},
		{"confluence", atlassian.Confluence, "conf"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	// PER SEAT, keyed by handle: Slack gives each agent its own app, and
	// the handle is what the URL path carries.
	if got.Slack["ceo"] != "ceo-signing" {
		t.Errorf("ceo's signing secret = %q", got.Slack["ceo"])
	}
	if _, present := got.Slack["cto"]; present {
		t.Error("a seat with no Slack app got an entry, which would open a route " +
			"that answers 401 instead of saying nothing is configured")
	}
}

func TestAnIntegrationLeftOutIsNotHalfConfigured(t *testing.T) {
	t.Parallel()
	// An absent block is what turns an integration OFF, and the secret it
	// would have carried must come back empty — that is what routes the
	// endpoint to a 503 rather than to a check against "".
	got := secretsFor(t, atlassianYAML)
	if got.GitHub != "" || got.GitLab != "" || got.Plane != "" || got.ForgeAppID != "" {
		t.Errorf("a config naming no self-hosted integration produced secrets: %+v", got)
	}
	if got.Slack != nil {
		t.Errorf("a company with no Slack apps produced a map: %v", got.Slack)
	}
}

func TestAHumanSeatGetsNoWebhookRoute(t *testing.T) {
	t.Parallel()
	// A human seat is addressable and never spawned, so nothing delivers
	// Events API traffic to one. Config refuses the combination outright,
	// but this function's contract is over an ORGANIZATION — which an
	// embedder builds directly — so the rule is enforced where it is read.
	organization := &org.Organization{
		Name: "Acme",
		Roles: []*org.Role{
			{Name: "CEO", DeclaredHandle: "ceo",
				Slack: org.SlackIdentity{SigningSecret: "agent-secret"}},
			{Name: "Founder", Kind: org.KindHuman, DeclaredHandle: "founder",
				Slack: org.SlackIdentity{SigningSecret: "human-secret"}},
		},
	}
	got := webhooks.SecretsOf(nil, organization)
	if got.Slack["ceo"] != "agent-secret" {
		t.Errorf("the agent seat lost its secret: %v", got.Slack)
	}
	if _, present := got.Slack["founder"]; present {
		t.Error("a human seat got a webhook route, so a delivery there would " +
			"be published addressed at a seat no node runs")
	}
}

func TestSecretsOfNothingIsEmptyRatherThanAPanic(t *testing.T) {
	t.Parallel()
	// Reached on a node mid-boot, and the answer must be "cannot verify"
	// rather than a crash on the request path.
	got := webhooks.SecretsOf(nil, nil)
	if got.GitHub != "" || got.Slack != nil || got.ForgeAppID != "" {
		t.Errorf("an empty epoch produced secrets: %+v", got)
	}
}

func TestARotatedSecretTakesEffectWithoutARestart(t *testing.T) {
	t.Parallel()
	// The secrets are read per request. A receiver that captured them at
	// startup would keep rejecting deliveries signed with a rotated secret
	// — a failure that looks exactly like an attack and clears only on
	// restart.
	e := newEdge(t)
	body := []byte(`{"action":"opened"}`)

	e.secrets.GitHub = "rotated"
	if got := e.post(t, "/webhooks/github", body, githubDelivery(body, "gh-secret")).Code; got != http.StatusUnauthorized {
		t.Errorf("the old secret still verifies: got %d", got)
	}
	if got := e.post(t, "/webhooks/github", body, githubDelivery(body, "rotated")).Code; got != http.StatusOK {
		t.Errorf("the rotated secret does not verify: got %d", got)
	}
}
