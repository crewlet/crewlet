package webhooks_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// ONE served config, where this file used to need two.
//
// A company can name only the three vendors this build wires — Mattermost for
// chat, GitLab for the code host, Plane for the tracker and the knowledge base
// — and of those three only GitLab and Plane put a secret in an epoch, because
// Mattermost has no inbound route at all: it holds a websocket open instead.
//
// The rest of the Secrets struct is no longer REACHABLE FROM A CONFIG. Jira,
// Confluence, GitHub, the Forge app id and every Slack app, org-level or per
// seat, are refused at validation with config.ErrUnimplemented
// (rewrite/decisions/703) — held below by TestNoConfigCanSupplyAnUnservedSecret,
// because that refusal is what keeps those routes answering 503. Their fields
// and the routes that read them stay, so the mapping is still held to filling
// them, over the only epoch that can still carry one: [unserved], built by hand.

// gitLabFixtureSecret is whsec_ over standard base64 of a 32-byte key — the
// only shape GitLab's API accepts, and so the only shape config validation
// lets past.
const gitLabFixtureSecret = "whsec_YS1maXh0dXJlLXNpZ25pbmcta2V5LW9mLTMyYnl0ZXM="

// rotatedGitLabSecret is a second, equally real one: a rotation gives the hook
// a new key, never a new shape.
const rotatedGitLabSecret = "whsec_YS1yb3RhdGVkLXNpZ25pbmcta2V5LW9mLTMyYnl0ZXM="

// servedYAML is a whole company on the three vendors this build serves.
var servedYAML = `
name: Acme
providers:
  llm:
    primary:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["key"]
integrations:
  mattermost:
    enabled: true
    url: https://mm.example.com
    team: acme
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: ` + gitLabFixtureSecret + `
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
      mattermost:
        bot_token: mm-ceo
  - name: CTO
    handle: cto
    llm: primary
`

// gitLabOnlyYAML names one integration and leaves the others out, which is
// what a company on a code host and no tracker yet actually looks like.
var gitLabOnlyYAML = `
name: Acme
providers:
  llm:
    primary:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["key"]
integrations:
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: ` + gitLabFixtureSecret + `
roles:
  - name: CEO
    handle: ceo
    llm: primary
`

// referencedSecretsYAML is the shape a real company has: every credential a
// ${VAR}, never a literal.
const referencedSecretsYAML = `
name: Acme
providers:
  llm:
    primary:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["key"]
integrations:
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
`

// The two fixtures this file used to parse, kept verbatim as what they now
// are: configs the engine refuses. See TestNoConfigCanSupplyAnUnservedSecret.
const refusedSelfHostedYAML = `
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

const refusedAtlassianYAML = `
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

// unserved is the verification material for the vendors this build validates
// and does not serve.
type unserved struct{ forgeAppID, github, jira, confluence, slack string }

// secrets reads that material out of the epoch a build serving them would
// have had.
//
// BUILT BY HAND, because no config produces one any more: every block below is
// refused with config.ErrUnimplemented, so a parsed company reaches SecretsOf
// with each of these fields empty and each of those routes therefore answering
// 503 — which is the whole point of the refusal (rewrite/decisions/703), and is
// asserted directly by TestNoConfigCanSupplyAnUnservedSecret.
//
// The mapping stays held all the same. The routes are still registered and
// still read these fields, an embedder still builds a Secrets directly, and the
// day one of those vendors ships its parser the field is what a delivery is
// verified against. A field wired to the wrong secret is invisible until a real
// delivery from a real vendor refuses to verify — with the vendor's settings
// page showing a healthy hook — so it is held here rather than dropped along
// with the fixtures that used to reach it.
func (u unserved) secrets(resolve func(string) string) webhooks.Secrets {
	company := &config.Company{
		Name: "Acme",
		Integrations: config.Integrations{
			ForgeAppID: u.forgeAppID,
			GitHub:     &config.GitHub{Enabled: true, WebhookSecret: u.github},
			Jira: &config.Jira{
				URL: "https://acme.atlassian.net", Token: "t",
				WebhookSecret: u.jira,
			},
			Confluence: &config.Confluence{
				URL: "https://acme.atlassian.net/wiki", Token: "t",
				WebhookSecret: u.confluence,
			},
		},
	}
	// The CEO holds a Slack app and the CTO does not, because the per-seat
	// map has to distinguish them.
	organization := &org.Organization{
		Name: "Acme",
		Roles: []*org.Role{
			{Name: "CEO", DeclaredHandle: "ceo",
				Slack: org.SlackIdentity{BotToken: "xoxb-1", SigningSecret: u.slack}},
			{Name: "CTO", DeclaredHandle: "cto"},
		},
	}
	return webhooks.SecretsOf(company, organization, resolve)
}

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
	served := secretsFor(t, servedYAML)
	unwired := unserved{
		forgeAppID: "forge-app", github: "gh", jira: "jira",
		confluence: "conf", slack: "ceo-signing",
	}.secrets(literal)

	for _, tc := range []struct{ name, got, want string }{
		{"gitlab", served.GitLab, gitLabFixtureSecret},
		{"plane", served.Plane, "pl"},
		{"github", unwired.GitHub, "gh"},
		{"jira", unwired.Jira, "jira"},
		{"confluence", unwired.Confluence, "conf"},
		{"forge app id", unwired.ForgeAppID, "forge-app"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	// PER SEAT, keyed by handle: Slack gives each agent its own app, and
	// the handle is what the URL path carries.
	if unwired.Slack["ceo"] != "ceo-signing" {
		t.Errorf("ceo's signing secret = %q", unwired.Slack["ceo"])
	}
	if _, present := unwired.Slack["cto"]; present {
		t.Error("a seat with no Slack app got an entry, which would open a route " +
			"that answers 401 instead of saying nothing is configured")
	}
	// A Mattermost seat opens no per-seat route: that transport holds a
	// websocket open rather than receiving deliveries, so this map stays
	// empty however many bots the company runs.
	if served.Slack != nil {
		t.Errorf("a company whose seats are on Mattermost got a per-seat "+
			"webhook map: %v", served.Slack)
	}
}

func TestAnIntegrationLeftOutIsNotHalfConfigured(t *testing.T) {
	t.Parallel()
	// An absent block is what turns an integration OFF, and the secret it
	// would have carried must come back empty — that is what routes the
	// endpoint to a 503 rather than to a check against "".
	got := secretsFor(t, gitLabOnlyYAML)
	if got.GitLab != gitLabFixtureSecret {
		t.Fatalf("the one integration this config names lost its secret: %q", got.GitLab)
	}
	if got.Plane != "" || got.GitHub != "" || got.Jira != "" ||
		got.Confluence != "" || got.ForgeAppID != "" {
		t.Errorf("a config naming only GitLab produced other integrations' "+
			"secrets: %+v", got)
	}
	if got.Slack != nil {
		t.Errorf("a company with no Slack apps produced a map: %v", got.Slack)
	}
}

// NO CONFIG CAN SUPPLY AN UNSERVED VENDOR'S SECRET, which is what makes the
// hand-built epoch in [unserved.secrets] the only one there is.
//
// This is the inversion of the two fixtures this file used to parse, kept as
// they were written. What they demonstrate now is worth what they used to:
// each is refused with ErrUnimplemented, so no delivery to those routes is
// ever verified in a build with no parser to hand it to. Held here rather than
// left to internal/config alone, because it is the reason THIS package may keep
// registering routes for vendors it cannot serve — take the refusal away and
// they come back to life with nothing behind them, silently, which is the
// failure d-703 exists to end.
func TestNoConfigCanSupplyAnUnservedSecret(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		yaml   string
		fields []string
	}{
		{"github, and the per-seat Slack app", refusedSelfHostedYAML,
			[]string{"integrations.github", "roles[0].integrations.slack"}},
		{"jira and confluence", refusedAtlassianYAML,
			[]string{"integrations.jira", "integrations.confluence"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseCompany([]byte(tc.yaml))
			if !errors.Is(err, config.ErrUnimplemented) {
				t.Fatalf("parse = %v, want config.ErrUnimplemented", err)
			}
			for _, field := range tc.fields {
				if !strings.Contains(err.Error(), field) {
					t.Errorf("the refusal never names %s, so an operator is "+
						"not told which block to remove: %v", field, err)
				}
			}
		})
	}
}

func TestAHumanSeatGetsNoWebhookRoute(t *testing.T) {
	t.Parallel()
	// A human seat is addressable and never spawned, so nothing delivers
	// Events API traffic to one. Config refuses a seat's Slack app outright
	// now, human seat or agent seat, so this organization is built directly
	// — which is what an embedder does anyway, and this function's contract
	// is over an ORGANIZATION, so the rule is enforced where it is read.
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
	//
	// Measured on GitLab because GitLab is a route a running company can
	// have a secret on at all: a vendor this build does not serve has no
	// config able to give it one, so nothing there is ever rotated.
	e := newEdge(t)
	body := []byte(`{"object_kind":"issue"}`)

	e.secrets.GitLab = rotatedGitLabSecret
	if got := e.post(t, "/webhooks/gitlab", body,
		gitlabDelivery(body, gitlabSecret, "msg_before_rotation", pinned)).Code; got != http.StatusUnauthorized {
		t.Errorf("the old secret still verifies: got %d", got)
	}
	if got := e.post(t, "/webhooks/gitlab", body,
		gitlabDelivery(body, rotatedGitLabSecret, "msg_after_rotation", pinned)).Code; got != http.StatusOK {
		t.Errorf("the rotated secret does not verify: got %d", got)
	}
}

// VERIFIABLE IS A CLAIM ABOUT THE RESOLVED VALUE, not about the document.
//
// A secret lives in the config as a ${VAR}, and the gap between "written
// down" and "resolved to something a route can check a signature with" is
// invisible from every other surface: the config shows a secret, the vendor's
// settings page shows a healthy hook, and every delivery is refused with
// nothing naming the variable.
func TestVerifiableNamesWhatCouldActuallyAcceptADelivery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   webhooks.Secrets
		want []string
	}{
		{"nothing configured", webhooks.Secrets{}, nil},
		{
			"a resolved gitlab key",
			webhooks.Secrets{GitLab: "whsec_YS1maXh0dXJlLXNpZ25pbmcta2V5LW9mLTMyYnl0ZXM="},
			[]string{"gitlab"},
		},
		{
			// THE CASE THIS EXISTS FOR. Non-empty and useless: the
			// reference reached the verifier verbatim, so a route with a
			// "configured" secret refuses every delivery.
			"gitlab holding an unresolved reference",
			webhooks.Secrets{GitLab: "${GITLAB_SIGNING_SECRET}"},
			nil,
		},
		{
			// GitLab signs with the DECODED 32 bytes. A value that is not
			// one cannot be the key for any delivery, however non-empty.
			"gitlab holding a key the vendor could not have produced",
			webhooks.Secrets{GitLab: "whsec_c2hvcnQ="},
			nil,
		},
		{
			"the surfaces whose secret is any non-empty string",
			webhooks.Secrets{GitHub: "gh", Plane: "pl", Jira: "jr", Confluence: "cf", ForgeAppID: "forge"},
			[]string{"confluence", "forge", "github", "jira", "plane"},
		},
		{
			// Mattermost holds a websocket rather than a route, and Slack's
			// material is per seat. Neither is a delivery this can verify.
			"a chat surface with no route to verify",
			webhooks.Secrets{Slack: map[string]string{"ceo": "s"}},
			nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.in.Verifiable()
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("Verifiable() = %v, want %v", got, tc.want)
			}
		})
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
		"${GL}": "gl-value", "${PL}": "pl-value", "${GH}": "gh-value",
		"${JR}": "jr-value", "${CF}": "cf-value", "${FORGE}": "forge-value",
		"${SLACK_CEO}": "slack-value",
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

	got := webhooks.SecretsOf(company, organization, resolve)
	unwired := unserved{
		forgeAppID: "${FORGE}", github: "${GH}", jira: "${JR}",
		confluence: "${CF}", slack: "${SLACK_CEO}",
	}.secrets(resolve)
	for label, value := range map[string]string{
		"gitlab": got.GitLab, "plane": got.Plane,
		"github": unwired.GitHub, "jira": unwired.Jira,
		"confluence": unwired.Confluence, "forge app id": unwired.ForgeAppID,
		"slack/ceo": unwired.Slack["ceo"],
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
	if got.GitLab != "" || got.Plane != "" {
		t.Errorf("secrets appeared with nothing to resolve them: %+v", got)
	}
	unwired := unserved{
		forgeAppID: "${FORGE}", github: "${GH}", jira: "${JR}",
		confluence: "${CF}", slack: "${SLACK_CEO}",
	}.secrets(nil)
	if unwired.GitHub != "" || unwired.Jira != "" || unwired.Confluence != "" ||
		unwired.ForgeAppID != "" || unwired.Slack != nil {
		t.Errorf("secrets appeared with nothing to resolve them: %+v", unwired)
	}
}
