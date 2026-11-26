package webhooks_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/crewlet/crewlet/internal/api/httpjson"
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

func TestAProductWithNoCloudIDHoldsItsDeliveries(t *testing.T) {
	t.Parallel()
	// No site, nothing to bind a token to: the same class as a missing
	// secret. Mutation: let an empty site list pass and this goes red.
	e := newEdge(t)
	e.secrets.ForgeSites = map[string]string{"jira": ownSite}

	res := postForge(t, e, forgePageEvent, validForgeClaims())
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", res.Code)
	}
	var answer map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if answer["error"] != string(httpjson.CodeNoWebhookSecret) {
		t.Errorf("error = %v, want %q", answer["error"], httpjson.CodeNoWebhookSecret)
	}
	if e.published.count() != 0 {
		t.Error("a delivery with no site to bind it to was published")
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
