package webhooks_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/atlassian"
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
	// One rule decides both: after the events, the answer holds exactly the
	// products whose events got 503. A product with no Cloud site at all
	// (Data Center, or no block) is refused; a url-named site binds through
	// its looked-up cloud id, or is held when the lookup fails. Mutation:
	// answer 503 whenever a product has no configured id, or hold a
	// url-named site statically, and a case goes red.
	const acme, down = "https://acme.atlassian.net", "https://down.atlassian.net"
	events := map[string][]byte{
		"jira": forgeJiraEvent, "confluence": forgePageEvent,
		"": []byte(`{"eventType":"avi:forge:other"}`),
	}
	for _, tc := range []struct {
		name string
		in   config.Integrations
		want map[string]int
	}{
		{"Data Center blocks", config.Integrations{
			Jira:       &config.Jira{URL: "https://jira.example.com"},
			Confluence: &config.Confluence{URL: "https://wiki.example.com"},
		}, map[string]int{"jira": 401, "confluence": 401, "": 401}},
		{"no blocks", config.Integrations{}, map[string]int{"jira": 401, "confluence": 401, "": 401}},
		{"a url-named site whose id is read", config.Integrations{
			Jira: &config.Jira{URL: acme},
		}, map[string]int{"jira": 200, "confluence": 401, "": 200}},
		{"a url-named site whose id cannot be read", config.Integrations{
			Jira: &config.Jira{URL: down},
		}, map[string]int{"jira": 503, "confluence": 401, "": 503}},
		{"a block with no address yet", config.Integrations{
			Confluence: &config.Confluence{},
		}, map[string]int{"jira": 401, "confluence": 503, "": 503}},
		{"cloud ids", config.Integrations{
			Jira: &config.Jira{CloudID: ownSite}, Confluence: &config.Confluence{CloudID: ownSite},
		}, map[string]int{"jira": 200, "confluence": 200, "": 200}},
		{"an unreadable site beside a cloud id", config.Integrations{
			Jira: &config.Jira{URL: down}, Confluence: &config.Confluence{CloudID: ownSite},
		}, map[string]int{"jira": 503, "confluence": 200, "": 200}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.in.ForgeAppID = "app-123"
			material := webhooks.SecretsOf(&config.Company{Integrations: tc.in}, nil, literal)
			sites := &siteIDs{ids: map[string]string{acme: ownSite}}
			for product, body := range events {
				e := newEdge(t, func(o *webhooks.Options) { o.CloudIDs = sites })
				*e.secrets = material
				if got := postForge(t, e, body, validForgeClaims()).Code; got != tc.want[product] {
					t.Errorf("event of %q: got %d, want %d", product, got, tc.want[product])
				}
			}
			// A product's 503 is a static hold config names, or a lookup
			// that failed: the two things a surface reports.
			static := material.IngressHolds()["forge"]
			for _, product := range []string{"jira", "confluence"} {
				name := map[string]string{"jira": "Jira", "confluence": "Confluence"}[product]
				_, lookupFailed := sites.Known(material.ForgeLookups[product])
				held := strings.Contains(static, "for "+name+" are refused") ||
					(material.ForgeLookups[product] != "" && lookupFailed != nil)
				if held != (tc.want[product] == 503) {
					t.Errorf("%s: held %v (static %q), route answered %d", product, held, static, tc.want[product])
				}
			}
		})
	}
}

// A URL-NAMED SITE BINDS THROUGH THE CLOUD ID ITS SITE NAMES: the site's own
// token is accepted, another site's refused. Mutation: skip the lookup's id
// in the binding and the first case goes red; accept any token once the
// lookup succeeds and the second does.
func TestAURLNamedSiteBindsThroughItsCloudID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		site string
		want int
	}{{ownSite, http.StatusOK}, {otherSite, http.StatusUnauthorized}} {
		e := newEdge(t, func(o *webhooks.Options) {
			o.CloudIDs = &siteIDs{ids: map[string]string{"https://acme.atlassian.net": ownSite}}
		})
		e.secrets.ForgeSites = nil
		e.secrets.ForgeLookups = map[string]string{"jira": "https://acme.atlassian.net"}
		if got := postForge(t, e, forgeJiraEvent, forgeClaimsFor(tc.site)).Code; got != tc.want {
			t.Errorf("a token for %s: got %d, want %d", tc.site, got, tc.want)
		}
	}
}

// siteIDs answers a site's cloud id from a table and fails for any other
// site, as an unreachable one would; Known is its last answer.
type siteIDs struct {
	mu   sync.Mutex
	ids  map[string]string
	last map[string]error
}

func (s *siteIDs) Resolve(_ context.Context, site string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[string]error{}
	}
	if id, ok := s.ids[site]; ok {
		s.last[site] = nil
		return id, nil
	}
	err := &atlassian.TenantInfoError{URL: site + "/_edge/tenant_info", Cause: "HTTP 502"}
	s.last[site] = err
	return "", err
}

func (s *siteIDs) Known(site string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.last[site]; err != nil {
		return "", err
	}
	return s.ids[site], nil
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

// A STATIC HOLD IS A BLOCK WITH NO ADDRESS YET, named with the field that
// clears it. A url-named site is a run-time hold, read from the passes'
// shared findings, never from config: so neither it, a cloud id, a Data
// Center block nor no app id holds anything here. Mutation: hold a url-named
// site statically and a case goes red.
func TestAStaticForgeHoldIsABlockWithNoAddressYet(t *testing.T) {
	t.Parallel()
	const addressless = "Forge relay events for Confluence are refused until " +
		"integrations.confluence.cloud_id (with site_url) is set"
	for _, tc := range []struct {
		name string
		in   config.Integrations
		want string
	}{
		{"a url-named site and a block with no address yet", config.Integrations{
			ForgeAppID: "app-123",
			Jira:       &config.Jira{URL: "https://down.atlassian.net"},
			Confluence: &config.Confluence{},
		}, addressless},
		{"a url-named site", config.Integrations{
			ForgeAppID: "app-123", Jira: &config.Jira{URL: "https://down.atlassian.net"},
		}, ""},
		{"a cloud id", config.Integrations{
			ForgeAppID: "app-123", Jira: &config.Jira{CloudID: ownSite},
		}, ""},
		{"a Data Center url", config.Integrations{
			ForgeAppID: "app-123", Jira: &config.Jira{URL: "https://jira.example.com"},
		}, ""},
		{"no forge app id", config.Integrations{
			Jira: &config.Jira{URL: "https://down.atlassian.net"}, Confluence: &config.Confluence{},
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
