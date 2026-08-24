package webhooks_test

import (
	"net/http"
	"strings"
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
    signing_secret: whsec_YS1maXh0dXJlLXNpZ25pbmcta2V5LW9mLTMyYnl0ZXM=
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

// referencedSecretsYAML is the shape a real company has: every credential a
// ${VAR}, never a literal. Split the same way the fixtures above are,
// because the knowledge backend is single-homed — Confluence and an enabled
// Plane are mutually exclusive.
const referencedSecretsYAML = `
name: Acme
providers:
  llm:
    primary:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["key"]
integrations:
  github:
    enabled: true
    webhook_secret: ${GH}
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: ${GL}
  plane:
    enabled: true
    url: https://plane.example.com
    workspace: acme
    webhook_secret: ${PL}
roles:
  - name: CEO
    handle: ceo
    llm: primary
    integrations:
      slack:
        bot_token: ${SLACK_CEO_TOKEN}
        signing_secret: ${SLACK_CEO}
`

const referencedAtlassianYAML = `
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
    webhook_secret: ${JR}
  confluence:
    url: https://acme.atlassian.net/wiki
    token: t
    webhook_secret: ${CF}
roles:
  - name: CEO
    handle: ceo
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
	return webhooks.SecretsOf(company, organization, literal)
}

func TestSecretsComeFromTheEpoch(t *testing.T) {
	t.Parallel()
	got := secretsFor(t, selfHostedYAML)
	atlassian := secretsFor(t, atlassianYAML)

	for _, tc := range []struct{ name, got, want string }{
		{"github", got.GitHub, "gh"},
		{"gitlab", got.GitLab, "whsec_YS1maXh0dXJlLXNpZ25pbmcta2V5LW9mLTMyYnl0ZXM="},
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
	got := webhooks.SecretsOf(nil, organization, literal)
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
	got := webhooks.SecretsOf(nil, nil, literal)
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

// literal is the resolver for the fixtures whose secrets are already plain
// values: it hands back what it was given.
func literal(value string) string { return value }

// A ${VAR} REACHES THE VERIFIER AS ITS VALUE, never as the reference.
//
// Secrets live in the config as references — verbatim in the stored payload,
// resolved at construction, which is what makes rotating one a change to the
// environment rather than to the company. This consumer did not resolve, and
// the result was seven routes verifying against the literal string
// "${GITLAB_SIGNING_SECRET}": every delivery from every vendor refused, with
// the vendor's settings page showing a healthy hook. Measured against a real
// GitLab.
//
// Worse than the outage is what the literal IS. A config field the dashboard
// renders is not a secret, so a forged delivery would have verified against a
// string an attacker could read.
func TestEveryVerifierGetsAResolvedSecret(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"${GH}": "gh-value", "${GL}": "gl-value", "${PL}": "pl-value",
		"${JR}": "jr-value", "${CF}": "cf-value", "${SLACK_CEO}": "slack-value",
		"${SLACK_CEO_TOKEN}": "xoxb-value",
	}
	resolve := func(v string) string {
		if got, ok := values[v]; ok {
			return got
		}
		return v
	}
	company, err := config.ParseCompany([]byte(referencedSecretsYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	organization, err := company.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	atlassian, err := config.ParseCompany([]byte(referencedAtlassianYAML))
	if err != nil {
		t.Fatalf("parse atlassian: %v", err)
	}

	got := webhooks.SecretsOf(company, organization, resolve)
	hosted := webhooks.SecretsOf(atlassian, nil, resolve)
	for label, value := range map[string]string{
		"github": got.GitHub, "gitlab": got.GitLab, "plane": got.Plane,
		"jira": hosted.Jira, "confluence": hosted.Confluence,
		"slack/ceo": got.Slack["ceo"],
	} {
		if strings.Contains(value, "${") {
			t.Errorf("%s verifies against the reference %q, not its value", label, value)
		}
		if value == "" {
			t.Errorf("%s resolved to nothing", label)
		}
	}
}

// NO RESOLVER MEANS NO SECRETS, and a route with nothing to verify with
// refuses to serve. The safe direction: a node mid-boot answers 503 rather
// than accepting whatever arrives.
func TestWithoutAResolverNothingVerifies(t *testing.T) {
	t.Parallel()
	company, err := config.ParseCompany([]byte(referencedSecretsYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := webhooks.SecretsOf(company, nil, nil)
	if got.GitLab != "" || got.GitHub != "" || got.Plane != "" {
		t.Errorf("secrets appeared with nothing to resolve them: %+v", got)
	}
}
