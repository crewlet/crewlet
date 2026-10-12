package config_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
)

// THE ROUTING FLOOR. An alert whose monitor names no owner has to go
// somewhere, and a company that named nowhere would have those alerts
// verified, counted and then delivered to nobody, which looks exactly like
// working coverage.
func TestDatadogRequiresARoutingFloor(t *testing.T) {
	t.Parallel()
	err := validateIntegrationDoc(t, "datadog", "    enabled: true\n    webhook_token: \"EXAMPLEDATADOGTOKEN0000000\"")
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
	err := validateIntegrationDoc(t, "datadog", "    enabled: true\n    route_to: swe")
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
		"    enabled: true\n    webhook_token: \"EXAMPLEDATADOGTOKEN0000000\"\n    route_to: swe")
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
    webhook_token: "EXAMPLEDATADOGTOKEN0000000"
    route_to: swe
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
				"    enabled: true\n    webhook_token: \"EXAMPLEDATADOGTOKEN0000000\"\n"+
					"    route_to: swe\n    handle_tag: \""+key+"\"")
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
		if got := in.WebhookBase(nil); got != want {
			t.Errorf("WebhookBase() = %q, want %q", got, want)
		}
	}
}

// A REFERENCE IS READ, NOT PASSED ON. `public_base_url` is a Tier B field, so
// a whole ${VAR} is a legal way to write it and the document stores it
// verbatim — but every caller of this method is building an address a
// third-party app will HOLD: a registered webhook, an app manifest an
// operator pastes, a redirect baked into an app at creation. Handed the
// reference itself, a hook is registered at "${PUBLIC_URL}/webhooks/gitlab",
// which the third-party app accepts, reports healthy, and delivers nowhere.
func TestTheWebhookBaseReadsAReference(t *testing.T) {
	t.Parallel()
	held := func(name string) (string, bool) {
		if name == "PUBLIC_URL" {
			return "https://crewlet.example.com/", true
		}
		return "", false
	}
	in := config.Integrations{PublicBaseURL: "${PUBLIC_URL}"}
	// Resolved, and trimmed afterwards: the trailing slash may come from the
	// stored value rather than from the document.
	if got := in.WebhookBase(held); got != "https://crewlet.example.com" {
		t.Errorf("WebhookBase() = %q, want the resolved address", got)
	}
}

// AND AN UNREADABLE ONE IS EMPTY, NEVER THE LITERAL.
//
// Empty is what every caller already reads as "this deployment has no inbound
// address", and it makes them refuse: no hook, no manifest, and a message
// naming the setting. The literal makes them all succeed — at building
// something nothing can reach. That failure was measured on the Atlassian
// pass, which sent `${ATLASSIAN_ORG_ID}` to Atlassian as an organization id.
func TestAnUnreadableWebhookBaseIsEmptyRatherThanTheReference(t *testing.T) {
	t.Parallel()
	none := func(string) (string, bool) { return "", false }
	for name, resolve := range map[string]func(string) (string, bool){
		"nothing holds it": none,
		"nothing to ask":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := config.Integrations{PublicBaseURL: "${PUBLIC_URL}"}
			if got := in.WebhookBase(resolve); got != "" {
				t.Errorf("WebhookBase() = %q, want \"\": a reference nothing "+
					"resolves must not reach a third-party app", got)
			}
		})
	}
}

// TWO ADDRESSES, NEITHER STANDING IN FOR THE OTHER. A vendor reaches the
// deployment at public_base_url and a person at dashboard_base_url, and with a
// public listener those are two sockets, each answering the other's routes 404:
// every webhook is built on the first and every link a person follows on the
// second, and an unset dashboard base composes no link rather than borrowing
// the vendor address, which is the wrong one exactly where the two differ.
func TestTheDashboardBaseIsNotTheWebhookBase(t *testing.T) {
	t.Parallel()
	held := func(name string) (string, bool) {
		if name == "DASHBOARD_URL" {
			return "https://crewlet.example.com/", true
		}
		return "", false
	}
	both := config.Integrations{
		PublicBaseURL:    "https://hooks.example.com",
		DashboardBaseURL: "${DASHBOARD_URL}",
	}
	if got := both.WebhookBase(held); got != "https://hooks.example.com" {
		t.Errorf("WebhookBase() = %q, want the vendor address", got)
	}
	if got := both.DashboardBase(held); got != "https://crewlet.example.com" {
		t.Errorf("DashboardBase() = %q, want the resolved dashboard address", got)
	}
	vendorOnly := config.Integrations{PublicBaseURL: "https://hooks.example.com"}
	if got := vendorOnly.DashboardBase(held); got != "" {
		t.Errorf("DashboardBase() = %q with no dashboard_base_url, want \"\": the "+
			"vendor address serves no dashboard behind a public listener", got)
	}
}

// A DASHBOARD BASE WITHOUT A SCHEME IS REFUSED, naming the field, for the
// reason a webhook base is: every link built on it is one nobody can follow.
func TestADashboardBaseWithoutASchemeIsRefused(t *testing.T) {
	t.Parallel()
	err := validateCompanyDoc(t, "integrations:\n  dashboard_base_url: crewlet.example.com\n")
	if err == nil || !strings.Contains(err.Error(), "integrations.dashboard_base_url") {
		t.Fatalf("a dashboard base with no scheme: %v", err)
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

// A URL WHOSE HOST IS A REFERENCE IS UNKNOWN, NOT DATA CENTER, in a company
// that declared nothing. Tier B keeps `${JIRA_URL}` verbatim, so validation
// sees no host at all; reading that as Data Center refused the guide's own
// example on the Forge relay for lacking a secret that route has no use for.
// A reference after a literal Cloud host is still Cloud, and one after a
// literal Data Center host is still asked (below).
func TestAReferencedAtlassianURLIsNotAskedForASigningSecret(t *testing.T) {
	t.Parallel()
	for _, block := range []string{"jira", "confluence"} {
		for _, url := range []string{`"${ATLASSIAN_URL}"`, `"https://acme.atlassian.net/${CONTEXT}"`} {
			t.Run(block+" "+url, func(t *testing.T) {
				t.Parallel()
				err := validateIntegrationDoc(t, block, "    url: "+url+"\n    token: t")
				if err != nil {
					t.Fatalf("a url whose deployment is unknown here was refused: %v", err)
				}
			})
		}
	}
}

// WHAT THE DOCUMENT SAYS IS NOT UNKNOWN. The branch that first exempted a
// referenced url exempted every url carrying a reference anywhere, so a Data
// Center company that said so in integrations.atlassian.deployment, or wrote
// its instance's host out and referenced only the context path, validated
// with no signing secret and was told nothing until the reconcile. The
// declaration decides what the host cannot, in both directions: Data Center
// is asked, and Cloud — an Isolated Cloud site at a base domain of its own
// included — is not. And a declaration of Data Center beside a host only
// Atlassian's Cloud answers on is refused, since no answer serves both.
func TestTheDeclaredDeploymentDecidesWhatTheURLCannot(t *testing.T) {
	t.Parallel()
	const (
		dataCenter = "    deployment: data_center"
		cloud      = "    deployment: cloud"
		// An organization block that says nothing about the deployment:
		// the form's default of Cloud is not a declaration, or every
		// company that wrote an organization id would have its
		// self-hosted Jira read as Cloud.
		silent = "    org_id: o-1"
	)
	for _, tc := range []struct {
		name, atlassian, url, wantErr string
	}{
		{"declared data center, referenced url", dataCenter, `"${ATLASSIAN_URL}"`, "webhook_secret"},
		{"nothing declared, referenced path", "", `"https://jira.corp.example.com/${CONTEXT}"`, "webhook_secret"},
		{"declared data center, referenced path", dataCenter, `"https://jira.corp.example.com/${CONTEXT}"`, "webhook_secret"},
		{"declared cloud, referenced url", cloud, `"${ATLASSIAN_URL}"`, ""},
		{"declared cloud, isolated cloud site", cloud, "https://acme.example-org.atlassian-isolated.net", ""},
		{"declared data center, cloud site", dataCenter, "https://acme.atlassian.net", "is an Atlassian Cloud address"},
		{"organization silent, self-hosted url", silent, "https://jira.corp.example.com", "webhook_secret"},
	} {
		for _, block := range []string{"jira", "confluence"} {
			t.Run(block+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				body := "    url: " + tc.url + "\n    token: t"
				name := block
				if tc.atlassian != "" {
					// The organization block sits beside the product's.
					name = "atlassian"
					body = tc.atlassian + "\n  " + block + ":\n" + body
				}
				err := validateIntegrationDoc(t, name, body)
				switch {
				case tc.wantErr == "" && err != nil:
					t.Fatalf("refused: %v", err)
				case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
					t.Fatalf("want a refusal naming %q, got %v", tc.wantErr, err)
				}
			})
		}
	}
}

// THE FALLBACK MUST NAME A SEAT THAT EXISTS.
//
// route_to is the only routing floor in this file, and a handle no seat has
// resolves to nothing: one `notification_undeliverable` warning per untagged
// alert, for ever, on a screen showing the configuration exactly as written.
// The parser deliberately passes an unknown handle through — a bad monitor
// TAG is the operator's typo and must stay visible — so the config document
// is the one place this can be caught.
func TestDatadogRefusesAFallbackNamingNoSeat(t *testing.T) {
	t.Parallel()
	err := validateIntegrationDoc(t, "datadog",
		strings.Replace(completeDatadog, "route_to: swe", "route_to: nobody-here", 1))
	if err == nil {
		t.Fatal("route_to naming no seat was accepted")
	}
	if !strings.Contains(err.Error(), "route_to") ||
		!strings.Contains(err.Error(), "nobody-here") {
		t.Errorf("error %q names neither the field nor the handle", err)
	}
	// AND IT SAYS WHO IS AVAILABLE, because "not a seat" without the
	// roster sends an operator to another file to find out what is.
	if !strings.Contains(err.Error(), "swe") {
		t.Errorf("error %q does not list the agent seats that do exist", err)
	}
}

// A HANDLE THAT IS NOT A HANDLE is refused for the shape rather than the
// roster, because "not an agent seat in this company" reads as a missing seat
// when the real answer is that no seat could ever be called this.
func TestDatadogRefusesAFallbackThatIsNotAHandle(t *testing.T) {
	t.Parallel()
	err := validateIntegrationDoc(t, "datadog",
		strings.Replace(completeDatadog, "route_to: swe", `route_to: "SRE Lead"`, 1))
	if err == nil {
		t.Fatal("route_to holding a name rather than a handle was accepted")
	}
	if !strings.Contains(err.Error(), "not a seat handle") {
		t.Errorf("error %q does not say the value is the wrong shape", err)
	}
}

// AND none STILL MEANS NOBODY. It is the one value that names no seat on
// purpose, so the roster check must not refuse it.
func TestDatadogAcceptsTheDismissFallback(t *testing.T) {
	t.Parallel()
	err := validateIntegrationDoc(t, "datadog",
		strings.Replace(completeDatadog, "route_to: swe", "route_to: none", 1))
	if err != nil {
		t.Fatalf("route_to: none was refused: %v", err)
	}
}

// THE REGION IS AS LOAD-BEARING AS THE TWO KEYS. A key issued in one region
// is refused by every other and the hostname is the only thing that tells
// them apart, so a block with both keys and no site cannot build one call —
// and it used to fail hours later as a dashboard finding rather than at load.
func TestDatadogRequiresTheRegionItsKeysWereIssuedIn(t *testing.T) {
	t.Parallel()
	err := validateIntegrationDoc(t, "datadog",
		strings.Replace(completeDatadog, "      site: datadoghq.com\n", "", 1))
	if err == nil {
		t.Fatal("a provisioning block with no site was accepted")
	}
	if !strings.Contains(err.Error(), "provisioning.site") {
		t.Errorf("error %q does not name the field", err)
	}
}

// And a region Datadog does not serve is refused rather than left to become a
// credential that authenticates nowhere.
func TestDatadogRefusesARegionItDoesNotServe(t *testing.T) {
	t.Parallel()
	err := validateIntegrationDoc(t, "datadog",
		strings.Replace(completeDatadog, "site: datadoghq.com", "site: datadoghq.co.uk", 1))
	if err == nil {
		t.Fatal("an unknown Datadog region was accepted")
	}
	if !strings.Contains(err.Error(), "datadoghq.co.uk") {
		t.Errorf("error %q does not quote the region it refused", err)
	}
}

// A ${VAR} IS UNKNOWN, NOT WRONG. Tier B holds pointers verbatim, so the
// membership check belongs where the client is built, on the resolved value.
func TestDatadogAcceptsAReferencedRegion(t *testing.T) {
	t.Parallel()
	err := validateIntegrationDoc(t, "datadog",
		strings.Replace(completeDatadog, "site: datadoghq.com", `site: "${DD_SITE}"`, 1))
	if err != nil {
		t.Fatalf("a ${VAR} region was refused: %v", err)
	}
}

// The region list here and the vendor client's own must agree, or a site this
// validator accepts is one datadog.NewClient refuses at the first call.
func TestTheRegionListMatchesTheVendorClient(t *testing.T) {
	t.Parallel()
	if got, want := config.DatadogSites, datadog.Sites(); !slices.Equal(got, want) {
		t.Errorf("config.DatadogSites = %v, datadog.Sites() = %v", got, want)
	}
}

// AND A HUMAN SEAT IS NOT AN ANSWER EITHER — the subtler of the two silent
// failures, because the seat EXISTS. The handle resolves, the configuration
// reads as correct on every screen, and notify.Deliverable then drops the
// delivery as a self-action, so every untagged alert lands nowhere. The
// engine's own fixture routed to a human seat until this rule existed.
func TestDatadogRefusesAFallbackNamingAHumanSeat(t *testing.T) {
	t.Parallel()
	doc := `
name: Acme
providers:
  llm:
    fast:
      type: anthropic
      model: claude-golden
integrations:
  datadog:
` + completeDatadog + `
roles:
  - name: SWE
    llm: fast
  - name: Founder
    kind: human
    contact:
      slack_user_id: U0FOUNDER
`
	// ParseCompany validates, so the refusal can come from either step.
	err := func() error {
		c, err := config.ParseCompany(
			[]byte(strings.Replace(doc, "route_to: swe", "route_to: founder", 1)))
		if err != nil {
			return err
		}
		return c.Validate()
	}()
	if err == nil {
		t.Fatal("route_to naming a human seat was accepted")
	}
	if !strings.Contains(err.Error(), "founder") ||
		!strings.Contains(err.Error(), "human seat") {
		t.Errorf("error %q does not say why a human seat cannot be the floor", err)
	}
}
