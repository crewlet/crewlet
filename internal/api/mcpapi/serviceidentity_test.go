package mcpapi_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A SIGN-IN TO A SERVICE IDENTITY IS REFUSED NAMING WHY: nobody signs in to a
// server whose tokens the engine is issued as it is used.
func TestASignInToAServiceIdentityIsRefusedNamingWhy(t *testing.T) {
	t.Parallel()
	signIns := &fakeSignIns{refusals: map[string]error{"vendor": mcpapi.ErrServiceIdentity}}
	svc := signInSurface(t, signIns, &recordedAudit{})
	status, body := call(t, svc, proved(time.Minute), http.MethodPost, "/mcp-servers/vendor/oauth")
	detail, _ := body["detail"].(string)
	if status != http.StatusConflict || body["error"] != "conflict" ||
		!strings.Contains(detail, "service identity") || !strings.Contains(detail, "client_credentials") {
		t.Fatalf("a sign-in to a service identity = %d %v", status, body)
	}
}

// oauthValuesDoc declares a service identity whose secret is written in the
// file, a person's grant's own client whose secret reads its own variable, and
// a published client document with no secret at all.
const oauthValuesDoc = `
name: Acme
mcp_servers:
  - name: vendor
    transport: http
    url: https://mcp.vendor.example.com/mcp
    oauth: {grant: client_credentials, client_id: crewlet-svc, client_secret: typed-in-the-file}
  - name: own
    transport: http
    url: https://mcp.own.example.com/mcp
    oauth: {client: own, client_id: crewlet-own, client_secret: "${OWN_CLIENT_SECRET}"}
  - name: public
    transport: http
    url: https://mcp.public.example.com/mcp
    oauth: {client: cimd}
`

// A CLIENT'S SECRET IS STORED FROM THE PAGE AS EVERY VALUE IS, AND TESTED WHERE
// A TEST SIGNS IN WITH IT: a service identity's is tested with the secret
// under test, sealed write-only and its field pointed at a variable of its
// own; a person's grant's own client's is refused a test (only a renewal sends
// it) and stored untested; a server that signs in with no secret is told so;
// and no answer carries the value.
//
// Mutation: let a person's grant's secret be tested, and its test passes on
// the grant's token whatever the secret is.
func TestAClientsSecretIsStoredWriteOnlyAndTestedWhereATestSignsInWithIt(t *testing.T) {
	t.Parallel()
	company, err := config.ParseCompany([]byte(oauthValuesDoc))
	if err != nil {
		t.Fatal(err)
	}
	checker, docs, store := &valueChecker{}, &fakeDocuments{company: company}, valueStore(t)
	svc := valueSurface(t, checker, docs, store, &recordedAudit{})
	admin := proved(time.Minute)
	path := func(server string) string { return "/mcp-servers/" + server + "/values/" + config.MCPValueClientSecret }

	code, raw := put(t, svc, admin, path("vendor"), valueBody(theValue))
	body := decoded(t, raw)
	variable := config.MCPValueVariable("vendor", config.MCPValueClientSecret, "")
	if code != http.StatusOK || body["variable"] != variable || body["tested"] != true || body["pointed"] != true {
		t.Fatalf("a service identity's secret = %d %v", code, body)
	}
	if strings.Contains(raw, theValue) {
		t.Fatalf("the answer carries the value: %s", raw)
	}
	asked := checker.requests()
	if last := asked[len(asked)-1]; last.Value == nil || last.Value.Key != config.MCPValueClientSecret ||
		last.Value.Variable != variable || last.Value.Value != theValue {
		t.Fatalf("tested %+v, want the secret under test", last)
	}
	row, found, err := store.Describe(t.Context(), variable)
	if err != nil || !found || !row.WriteOnly || row.Source != secrets.SourceMCP {
		t.Fatalf("the sealed row = %+v (%v, %v), want it write-only from mcp", row, found, err)
	}
	if got := docs.company.MCPServers[0].OAuth.ClientSecret; got != "${"+variable+"}" {
		t.Fatalf("the client secret reads %q", got)
	}
	values, _ := body["values"].([]any)
	if len(values) != 1 || values[0].(map[string]any)["as"] != string(config.MCPValueOAuth) ||
		values[0].(map[string]any)["set"] != true {
		t.Fatalf("the server's values are %v", body["values"])
	}

	tests := len(checker.requests())
	code, raw = put(t, svc, admin, path("own"), valueBody(theValue))
	if body := decoded(t, raw); code != http.StatusBadRequest || body["error"] != "invalid_query" ||
		!strings.Contains(body["detail"].(string), "test=false") {
		t.Fatalf("a tested secret of a person's grant = %d %v", code, body)
	}
	if _, found, _ := store.Describe(t.Context(), "OWN_CLIENT_SECRET"); found || len(checker.requests()) != tests {
		t.Fatal("a refused secret was tested or stored")
	}
	code, raw = put(t, svc, admin, path("own")+"?test=false", valueBody(theValue))
	if body := decoded(t, raw); code != http.StatusOK || body["variable"] != "OWN_CLIENT_SECRET" ||
		body["pointed"] != false {
		t.Fatalf("an untested secret of a person's grant = %d %v", code, body)
	}
	if row, found, _ := store.Describe(t.Context(), "OWN_CLIENT_SECRET"); !found || !row.WriteOnly {
		t.Fatalf("the person's grant's secret = %+v, %v", row, found)
	}

	code, raw = put(t, svc, admin, path("public"), valueBody(theValue))
	if body := decoded(t, raw); code != http.StatusConflict || !strings.Contains(body["detail"].(string), "no client secret") {
		t.Fatalf("a secret for a public client = %d %v", code, body)
	}
}
