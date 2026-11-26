package webhooks_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/config"
)

// The cloud ids of the company's own Atlassian site and of another site the
// same app is installed on.
const (
	ownSite   = "0f3a6c1e-0000-4000-8000-00000000c0de"
	otherSite = "7b21d9aa-0000-4000-8000-0000000051de"
)

var forgeJiraEvent = []byte(`{"eventType":"avi:jira:created:issue","atlassianId":"acct-4",
  "issue":{"key":"OPS-7","fields":{"summary":"Rotate the keys"}}}`)

func postForge(t *testing.T, e *edge, body []byte, claims jwt.MapClaims) *httptest.ResponseRecorder {
	t.Helper()
	token := forgeToken(t, claims, jwt.SigningMethodRS256, forgeKID, forgeKey)
	return e.post(t, "/webhooks/forge", body, forgeHeaders(token))
}

func TestAForgeTokenMintedForAnotherSiteIsRefused(t *testing.T) {
	t.Parallel()
	// Mutation: drop the bindSite call from the route and this goes red.
	// The token is genuine and for the right app; only its site is wrong.
	for _, body := range [][]byte{forgePageEvent, forgeJiraEvent} {
		e := newEdge(t)
		res := postForge(t, e, body, forgeClaimsFor(otherSite))
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401 for a token minted for another site", res.Code)
		}
		if e.published.count() != 0 || len(e.rows(t)) != 0 {
			t.Error("a token for another site reached the queue or the store")
		}
	}
}

func TestAForgeTokenNamesItsSiteInAnyOfItsSignedClaims(t *testing.T) {
	t.Parallel()
	// Atlassian marks a context's cloudId optional, so the gateway paths
	// name the site as well. Mutation: read only contexts[].cloudId and the
	// two apiBaseUrl cases go red.
	gateway := "https://api.atlassian.com/ex/confluence/" + ownSite
	app := func(app map[string]any) jwt.MapClaims {
		c := validForgeClaims()
		c["app"] = app
		return c
	}
	cases := []struct {
		name   string
		claims jwt.MapClaims
		want   int
	}{
		{"a context's cloudId", app(map[string]any{"installation": map[string]any{
			"contexts": []any{map[string]any{"cloudId": ownSite}}}}), http.StatusOK},
		{"a context's apiBaseUrl", app(map[string]any{"installation": map[string]any{
			"contexts": []any{map[string]any{"apiBaseUrl": gateway}}}}), http.StatusOK},
		{"the app's apiBaseUrl", app(map[string]any{"apiBaseUrl": gateway}), http.StatusOK},
		{"an upper-case cloud id", forgeClaimsFor(strings.ToUpper(ownSite)), http.StatusOK},
		{"no site at all", app(map[string]any{}), http.StatusUnauthorized},
		// An installation is on one site. Mutation: match on any named id
		// and this case goes red, since its context names the own site.
		{"claims naming two sites", app(map[string]any{
			"apiBaseUrl": "https://api.atlassian.com/ex/jira/" + otherSite,
			"installation": map[string]any{
				"contexts": []any{map[string]any{"cloudId": ownSite}}}}), http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEdge(t)
			if got := postForge(t, e, forgePageEvent, tc.claims).Code; got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestTheBodysContextDoesNotRescueATokenForAnotherSite(t *testing.T) {
	t.Parallel()
	// The body's context names the company's site, and the token does not
	// sign the body. Mutation: take the site from context.installContext
	// or context.workspaceId and this goes red.
	e := newEdge(t)
	body := []byte(`{"eventType":"avi:jira:created:issue","atlassianId":"acct-5",
	  "issue":{"key":"OPS-8"},
	  "context":{"installContext":"ari:cloud:jira::site/` + ownSite + `",
	             "workspaceId":"` + ownSite + `","cloudId":"` + ownSite + `"}}`)
	if got := postForge(t, e, body, forgeClaimsFor(otherSite)).Code; got != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", got)
	}
	if e.published.count() != 0 {
		t.Error("an unsigned context vouched for a token minted for another site")
	}
}

func TestEachProductIsHeldToItsOwnSite(t *testing.T) {
	t.Parallel()
	// Jira and Confluence may be on different sites. Mutation: accept a
	// token naming either configured site and the crossed cases go red.
	cases := []struct {
		name string
		body []byte
		site string
		want int
	}{
		{"jira event, jira's site", forgeJiraEvent, ownSite, http.StatusOK},
		{"jira event, confluence's site", forgeJiraEvent, otherSite, http.StatusUnauthorized},
		{"confluence event, confluence's site", forgePageEvent, otherSite, http.StatusOK},
		{"confluence event, jira's site", forgePageEvent, ownSite, http.StatusUnauthorized},
		// An event of neither product may come from either site.
		{"other event, jira's site", []byte(`{"eventType":"avi:forge:other"}`), ownSite, http.StatusOK},
		{"other event, confluence's site", []byte(`{"eventType":"avi:forge:other"}`), otherSite, http.StatusOK},
		{"other event, a third site", []byte(`{"eventType":"avi:forge:other"}`), "a-third-site", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEdge(t)
			e.secrets.ForgeSites = map[string]string{"jira": ownSite, "confluence": otherSite}
			res := postForge(t, e, tc.body, forgeClaimsFor(tc.site))
			if res.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", res.Code, tc.want, res.Body)
			}
		})
	}
}

func TestTheRouteHoldsExactlyWhatTheIntegrationsAnswerHolds(t *testing.T) {
	t.Parallel()
	// One rule decides both: a 503 for a product the answer shows held, and
	// for no other. A product with no Cloud site that is not held (Data
	// Center, or no block) is refused. Mutation: answer 503 whenever a
	// product has no site, or decide it from anything but ForgeHeld, and a
	// case goes red.
	events := map[string][]byte{
		"jira": forgeJiraEvent, "confluence": forgePageEvent,
		"": []byte(`{"eventType":"avi:forge:other"}`),
	}
	for _, tc := range []struct {
		name string
		in   config.Integrations
	}{
		{"Data Center blocks", config.Integrations{
			Jira:       &config.Jira{URL: "https://jira.example.com"},
			Confluence: &config.Confluence{URL: "https://wiki.example.com"},
		}},
		{"no blocks", config.Integrations{}},
		{"a url-named Cloud site", config.Integrations{
			Jira: &config.Jira{URL: "https://acme.atlassian.net"},
		}},
		{"a block with no address yet", config.Integrations{
			Confluence: &config.Confluence{},
		}},
		{"cloud ids", config.Integrations{
			Jira: &config.Jira{CloudID: ownSite}, Confluence: &config.Confluence{CloudID: ownSite},
		}},
		{"one of each", config.Integrations{
			Jira: &config.Jira{URL: "https://acme.atlassian.net"}, Confluence: &config.Confluence{CloudID: ownSite},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.in.ForgeAppID = "app-123"
			material := webhooks.SecretsOf(&config.Company{Integrations: tc.in}, nil, literal)
			held := map[string]bool{}
			for _, hold := range material.ForgeHeld {
				held[hold.Product] = true
			}
			if (len(held) > 0) != (material.IngressHolds()["forge"] != "") {
				t.Fatalf("ForgeHeld %v and IngressHolds %v disagree", material.ForgeHeld, material.IngressHolds())
			}
			for product, body := range events {
				e := newEdge(t)
				*e.secrets = material
				_, site := material.ForgeSites[product]
				want := http.StatusUnauthorized
				switch {
				case product == "" && len(material.ForgeSites) > 0, site:
					want = http.StatusOK
				case held[product], product == "" && len(held) > 0:
					want = http.StatusServiceUnavailable
				}
				if got := postForge(t, e, body, validForgeClaims()).Code; got != want {
					t.Errorf("event of %q: got %d, want %d", product, got, want)
				}
			}
		})
	}
}

func TestSecretsOfReadsEachProductsCloudID(t *testing.T) {
	t.Parallel()
	// The token names a site by cloud id only, so only a block naming one
	// binds. Mutation: read a gateway url's host, or bind a site url or a
	// Data Center url, and the matching case goes red.
	got := secretsFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    primary:
      model: claude-sonnet-5
integrations:
  jira:
    cloud_id: `+strings.ToUpper(ownSite)+`
    site_url: https://acme.atlassian.net
    token: t
  confluence:
    url: https://acme.atlassian.net/wiki
    token: t
  forge_app_id: forge-app
roles:
  - name: CEO
    handle: ceo
    llm: primary
`).ForgeSites
	if len(got) != 1 || got["jira"] != ownSite {
		t.Errorf("ForgeSites = %v, want jira's cloud id alone: a site url names no cloud id", got)
	}

	gateway := secretsFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    primary:
      model: claude-sonnet-5
integrations:
  jira:
    url: https://api.atlassian.com/ex/jira/`+ownSite+`
    token: t
roles:
  - name: CEO
    handle: ceo
    llm: primary
`).ForgeSites
	if gateway["jira"] != ownSite {
		t.Errorf("a gateway url gave %v, want the cloud id in its path", gateway)
	}

	if sites := secretsFor(t, servedYAML).ForgeSites; len(sites) != 0 {
		t.Errorf("Data Center urls gave cloud ids %v", sites)
	}
}

// A HELD PRODUCT IS NAMED WITH THE FIELD THAT CLEARS IT, in the words the
// route logs and the integrations answer shows, and they depend on the block:
// config refuses a url beside a cloud_id, so a url-named block is told to
// replace its url. Mutation: hold a block with a cloud id, a Data Center
// block, or any block with no app id, or word both holds alike, and a case
// goes red.
func TestAForgeHoldNamesTheFieldThatClearsIt(t *testing.T) {
	t.Parallel()
	const (
		urlNamed = "Forge relay events for Jira are refused until integrations.jira.url " +
			"is replaced by cloud_id (with site_url)"
		addressless = "Forge relay events for Confluence are refused until " +
			"integrations.confluence.cloud_id (with site_url) is set"
	)
	for _, tc := range []struct {
		name string
		in   config.Integrations
		want string
	}{
		{"a url-named Cloud site and a block with no address yet", config.Integrations{
			ForgeAppID: "app-123",
			Jira:       &config.Jira{URL: "https://acme.atlassian.net"},
			Confluence: &config.Confluence{},
		}, urlNamed + "; " + addressless},
		{"a cloud id", config.Integrations{
			ForgeAppID: "app-123", Jira: &config.Jira{CloudID: ownSite},
		}, ""},
		{"a Data Center url", config.Integrations{
			ForgeAppID: "app-123", Jira: &config.Jira{URL: "https://jira.example.com"},
		}, ""},
		{"no forge app id", config.Integrations{
			Jira: &config.Jira{URL: "https://acme.atlassian.net"}, Confluence: &config.Confluence{},
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := webhooks.SecretsOf(&config.Company{Integrations: tc.in}, nil, literal).IngressHolds()
			if got["forge"] != tc.want || (tc.want == "") != (len(got) == 0) {
				t.Errorf("IngressHolds() = %v, want forge: %q", got, tc.want)
			}
		})
	}
}
