package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/jira"
)

// THE ROUTING FLOOR. An alert whose monitor names no owner has to go
// somewhere, and a company that named nowhere would have those alerts
// verified, counted and then delivered to nobody, which looks exactly like
// working coverage.
func TestDatadogRequiresARoutingFloor(t *testing.T) {
	t.Parallel()
	err := validateIntegrationDoc(t, "datadog", "    enabled: true\n    webhook_token: \"whsec_x\"")
	if err == nil {
		t.Fatal("an enabled Datadog block with no route_to was accepted")
	}
	if !strings.Contains(err.Error(), "route_to") {
		t.Errorf("error %q does not name the field", err)
	}
}

// The token is still required, and the two refusals are independent: a
// company that fixed one must still be told about the other.
func TestDatadogRequiresItsToken(t *testing.T) {
	t.Parallel()
	err := validateIntegrationDoc(t, "datadog", "    enabled: true\n    route_to: sre-lead")
	if err == nil {
		t.Fatal("an enabled Datadog block with no webhook_token was accepted")
	}
	if !strings.Contains(err.Error(), "webhook_token") {
		t.Errorf("error %q does not name the field", err)
	}
}

// AND THE KEYS THAT REGISTER THE WEBHOOK ARE REQUIRED TOO.
//
// Datadog posts to whatever URL its Webhooks integration holds, and the
// engine writes that URL with this credential pair. An enabled block without
// them serves a route, checks a token, reports itself connected and receives
// nothing, because nothing at Datadog was ever told this deployment exists.
func TestDatadogRequiresTheKeysThatRegisterItsWebhook(t *testing.T) {
	t.Parallel()
	err := validateIntegrationDoc(t, "datadog",
		"    enabled: true\n    webhook_token: \"whsec_x\"\n    route_to: sre-lead")
	if err == nil {
		t.Fatal("an enabled Datadog block with no provisioning keys was accepted")
	}
	if !strings.Contains(err.Error(), "provisioning") {
		t.Errorf("error %q does not name the field", err)
	}
}

// A complete block validates, so the rules above are requirements rather than
// a refusal of everything.
func TestACompleteDatadogBlockValidates(t *testing.T) {
	t.Parallel()
	err := validateIntegrationDoc(t, "datadog", completeDatadog)
	if err != nil {
		t.Fatalf("a complete Datadog block was refused: %v", err)
	}
}

// completeDatadog is a block with every requirement answered.
const completeDatadog = `    enabled: true
    webhook_token: "whsec_x"
    route_to: sre-lead
    provisioning:
      site: datadoghq.com
      api_key: "${DD_API_KEY}"
      app_key: "${DD_APP_KEY}"`

// A WEBHOOK NAME BECOMES A HANDLE, and any of these characters ends that
// handle early: a monitor naming "@webhook-my hook" reaches nobody.
func TestDatadogRefusesAWebhookNameThatCannotBeAHandle(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"my hook", "crew@let", "a,b"} {
		err := validateIntegrationDoc(t, "datadog",
			completeDatadog+"\n    webhook_name: \""+name+"\"")
		if err == nil {
			t.Errorf("webhook_name %q was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "webhook_name") {
			t.Errorf("error %q does not name the field", err)
		}
	}
}

// A DISABLED block is not checked, so an operator can keep a configured
// integration switched off without having to keep it complete.
func TestADisabledDatadogBlockIsNotChecked(t *testing.T) {
	t.Parallel()
	if err := validateIntegrationDoc(t, "datadog", "    enabled: false"); err != nil {
		t.Fatalf("a disabled Datadog block was refused: %v", err)
	}
}

// A tag key holding a colon, a comma or a space never matches a monitor:
// Datadog uses the colon to separate a key from its value and the comma to
// separate one tag from the next. Accepted, the integration would route
// nothing and say nothing.
func TestDatadogRefusesATagKeyThatCannotMatch(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"crewlet:owner", "a,b", "two words"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			err := validateIntegrationDoc(t, "datadog",
				"    enabled: true\n    webhook_token: \"whsec_x\"\n"+
					"    route_to: sre-lead\n    handle_tag: \""+key+"\"")
			if err == nil {
				t.Fatalf("handle_tag %q was accepted", key)
			}
			if !strings.Contains(err.Error(), "handle_tag") {
				t.Errorf("error %q does not name the field", err)
			}
		})
	}
}

// THE RESTATED CONSTANT. config is a leaf the vendor packages depend on, so
// the default tag key is spelled here as well as in internal/datadog. Two
// spellings would route on one key and document the other, and every alert
// would reach the fallback while looking correctly configured.
func TestTheDefaultHandleTagAgreesWithTheVendorPackage(t *testing.T) {
	t.Parallel()
	var block config.Datadog
	if got := block.HandleTagOrDefault(); got != datadog.DefaultHandleTag {
		t.Fatalf("config defaults the tag key to %q, the vendor package to %q",
			got, datadog.DefaultHandleTag)
	}
}

// A key an operator set wins, and is folded to lower because Datadog
// lowercases tag keys on ingestion.
func TestAConfiguredHandleTagIsUsedAndFolded(t *testing.T) {
	t.Parallel()
	block := config.Datadog{HandleTag: "  Owner  "}
	if got := block.HandleTagOrDefault(); got != "owner" {
		t.Fatalf("the tag key is %q, want %q", got, "owner")
	}
}

// THE ADDRESS A VENDOR REACHES THIS DEPLOYMENT ON is refused here rather than
// discovered by the third-party app. Every webhook URL is built on it, so a value
// missing its scheme registers a hook the third-party app reports as healthy and
// delivers nowhere, which is the exact failure this field exists to close.
func TestAPublicBaseURLWithoutASchemeIsRefused(t *testing.T) {
	t.Parallel()
	for _, base := range []string{"crewlet.example.com", "//crewlet.example.com"} {
		t.Run(base, func(t *testing.T) {
			t.Parallel()
			err := validateCompanyDoc(t, "integrations:\n  public_base_url: \""+base+"\"")
			if err == nil {
				t.Fatalf("public_base_url %q was accepted", base)
			}
			if !strings.Contains(err.Error(), "public_base_url") {
				t.Errorf("error %q does not name the field", err)
			}
		})
	}
}

// And a real one is accepted, so the rule is a check rather than a refusal of
// everything.
func TestAPublicBaseURLWithASchemeIsAccepted(t *testing.T) {
	t.Parallel()
	err := validateCompanyDoc(t, "integrations:\n  public_base_url: \"https://crewlet.example.com\"")
	if err != nil {
		t.Fatalf("a valid public_base_url was refused: %v", err)
	}
}

// A TRAILING SLASH IS TRIMMED ONCE, here, rather than by each of the five
// callers that build a URL on it. A base ending in "/" yields
// "…//webhooks/jira", which some third-party apps normalise, some reject, and some
// accept while signing the unnormalised form.
func TestTheWebhookBaseIsTrimmed(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"https://crewlet.example.com/":   "https://crewlet.example.com",
		"https://crewlet.example.com///": "https://crewlet.example.com",
		"  https://crewlet.example.com ": "https://crewlet.example.com",
		"":                               "",
	}
	for in, want := range cases {
		in := config.Integrations{PublicBaseURL: in}
		if got := in.WebhookBase(); got != want {
			t.Errorf("WebhookBase() = %q, want %q", got, want)
		}
	}
}

// validateCompanyDoc parses and validates a company document carrying body at
// the top level.
func validateCompanyDoc(t *testing.T, body string) error {
	t.Helper()
	doc := `
name: Acme
providers:
  llm:
    fast:
      type: anthropic
      model: claude-golden
` + body + `
roles:
  - name: SWE
    llm: fast
`
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		return err
	}
	return c.Validate()
}

// A CLOUD SITE GIVEN BY URL IS CLOUD, and is not asked for a signing secret
// it cannot use. Both validators used to decide "Cloud" from cloud_id alone,
// so https://acme.atlassian.net, which is how most companies write a Cloud
// site, was treated as Data Center and refused for lacking webhook_secret.
// Measured against a live Cloud site: the Jira hook there signs with the
// secret when given one, and the Confluence hooks carry a token instead, so
// neither is a field a Cloud config can be forced to hold.
func TestAnAtlassianCloudSiteByURLIsNotAskedForASigningSecret(t *testing.T) {
	t.Parallel()
	for _, block := range []string{"jira", "confluence"} {
		t.Run(block, func(t *testing.T) {
			t.Parallel()
			err := validateIntegrationDoc(t, block,
				"    url: https://acme.atlassian.net\n    token: t")
			if err != nil {
				t.Fatalf("a Cloud site by URL with no webhook_secret was refused: %v", err)
			}
		})
	}
}

// And a self-hosted address still is, because there the secret is the
// route's only credential.
func TestADataCenterURLStillNeedsASigningSecret(t *testing.T) {
	t.Parallel()
	for _, block := range []string{"jira", "confluence"} {
		t.Run(block, func(t *testing.T) {
			t.Parallel()
			err := validateIntegrationDoc(t, block,
				"    url: https://wiki.corp.example.com\n    token: t")
			if err == nil || !strings.Contains(err.Error(), "webhook_secret") {
				t.Fatalf("a Data Center site without webhook_secret was accepted: %v", err)
			}
		})
	}
}

// The host rule here and the vendor clients' own must agree, or a site the
// validator calls Cloud is one the client registers a Data Center hook on.
func TestTheCloudHostRuleMatchesTheVendorClients(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{
		"https://acme.atlassian.net", "https://acme.atlassian.net/wiki",
		"https://acme.jira.com", "https://api.atlassian.com/ex/confluence/abc",
		"https://wiki.corp.example.com", "http://localhost:8090",
	} {
		fromConfig := config.IsAtlassianCloud(addr)
		fromConfluence := confluence.DeploymentOf(addr) == confluence.Cloud
		fromJira := jira.DeploymentOf(addr) == jira.Cloud
		if fromConfig != fromConfluence || fromConfig != fromJira {
			t.Errorf("%s: config=%v confluence=%v jira=%v", addr, fromConfig, fromConfluence, fromJira)
		}
	}
}
