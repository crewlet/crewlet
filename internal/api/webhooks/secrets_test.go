package webhooks_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// ONE served config, where this file used to need two.
//
// EVERY FIELD IS REACHABLE FROM A CONFIG NOW, which is what these fixtures
// are: whole companies, parsed, with the secret each route verifies against
// read back out. Mattermost is the one integration that contributes none —
// it has no inbound route at all, holding a websocket open instead — and
// that absence is asserted rather than assumed.
//
// A field wired to the wrong secret is invisible until a real delivery from a
// real vendor refuses to verify, with the vendor's settings page showing a
// healthy hook. That is what the mapping is held to here.

// gitLabFixtureSecret is whsec_ over standard base64 of a 32-byte key — the
// only shape GitLab's API accepts, and so the only shape config validation
// lets past.
const gitLabFixtureSecret = "whsec_YS1maXh0dXJlLXNpZ25pbmcta2V5LW9mLTMyYnl0ZXM="

// rotatedGitLabSecret is a second, equally real one: a rotation gives the hook
// a new key, never a new shape.
const rotatedGitLabSecret = "whsec_YS1yb3RhdGVkLXNpZ25pbmcta2V5LW9mLTMyYnl0ZXM="

// servedYAML is a whole company on the vendors this build serves.
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
  jira:
    url: https://jira.example.com
    token: t
    webhook_secret: jr
  github:
    enabled: true
    webhook_secret: gh
  forge_app_id: forge-app
roles:
  - name: CEO
    handle: ceo
    llm: primary
    integrations:
      mattermost:
        bot_token: mm-ceo
      slack:
        bot_token: xoxb-1
        signing_secret: ceo-signing
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
  jira:
    url: https://jira.example.com
    token: t
    webhook_secret: ${JR}
  github:
    enabled: true
    webhook_secret: ${GH}
  forge_app_id: ${FORGE}
roles:
  - name: CEO
    handle: ceo
    llm: primary
`

// confluenceYAML is the knowledge base on its own. It cannot join the
// fixture above: the knowledge backend is single-homed, and that config
// already runs Plane.
var confluenceYAML = `
name: Acme
providers:
  llm:
    primary:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["key"]
integrations:
  confluence:
    url: https://wiki.example.com
    token: t
    webhook_secret: cf
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
	served := secretsFor(t, servedYAML)

	for _, tc := range []struct{ name, got, want string }{
		{"gitlab", served.GitLab, gitLabFixtureSecret},
		{"plane", served.Plane, "pl"},
		{"jira", served.Jira, "jr"},
		{"confluence", secretsFor(t, confluenceYAML).Confluence, "cf"},
		{"forge app id", served.ForgeAppID, "forge-app"},
		{"github", served.GitHub, "gh"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	// PER SEAT, keyed by handle: Slack gives each agent its own app, and
	// the handle is what the URL path carries.
	if served.Slack["ceo"] != "ceo-signing" {
		t.Errorf("ceo's signing secret = %q", served.Slack["ceo"])
	}
	if _, present := served.Slack["cto"]; present {
		t.Error("a seat with no Slack app got an entry, which would open a route " +
			"that answers 401 instead of saying nothing is configured")
	}
	// A MATTERMOST SEAT OPENS NO PER-SEAT ROUTE, even in a company that
	// also runs Slack: that transport holds a websocket open rather than
	// receiving deliveries, so only the Slack apps appear here. The CEO
	// carries both identities in this fixture, which is what makes the
	// point — one entry, not two.
	if len(served.Slack) != 1 {
		t.Errorf("a company on both chat surfaces got %d per-seat route(s): %v",
			len(served.Slack), served.Slack)
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
	for label, value := range map[string]string{
		"gitlab": got.GitLab, "plane": got.Plane,
		"jira": got.Jira, "forge app id": got.ForgeAppID,
		"github": got.GitHub,
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
	if got.GitLab != "" || got.Plane != "" || got.Jira != "" || got.ForgeAppID != "" ||
		got.Confluence != "" || got.GitHub != "" {
		t.Errorf("secrets appeared with nothing to resolve them: %+v", got)
	}
}
