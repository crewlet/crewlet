package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mcp"
)

// twoKeyServer is a Streamable HTTP MCP endpoint that, as Datadog's does,
// answers 401 to any request missing either the organization's API key or the
// agent's application key.
func twoKeyServer(t *testing.T, apiKey, appKey string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("DD_API_KEY") != apiKey || r.Header.Get("DD_APPLICATION_KEY") != appKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"errors":["Unauthorized"]}`)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result string
		switch req.Method {
		case "initialize":
			result = fmt.Sprintf(`{"protocolVersion":%q,"capabilities":{"tools":{"listChanged":false}},`+
				`"serverInfo":{"name":"two-key","version":"0.0.1"}}`, req.Params.ProtocolVersion)
		case "tools/list":
			result = `{"tools":[{"name":"search_logs","description":"Search logs.","inputSchema":{"type":"object"}}]}`
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(req.ID)+
				`,"error":{"code":-32601,"message":"unknown"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(req.ID)+`,"result":`+result+`}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// datadogSeatDoc is a company on Datadog whose agent ops is on at it, with a
// per-agent Datadog server carrying headers.
func datadogSeatDoc(headers string) string {
	return `
name: Acme
seat_access_migrated: [datadog]
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  datadog:
    enabled: true
    webhook_token: EXAMPLEDATADOGTOKEN0000000
    route_to: ops
    provisioning:
      site: datadoghq.com
      api_key: "${DATADOG_API_KEY}"
      app_key: "${DATADOG_APP_KEY}"
mcp_servers:
  - name: datadog
    tools: {default: allow}
    transport: http
    shared: false
    url: "https://mcp.datadoghq.com/v1/mcp"` + headers + `
roles:
  - name: Ops
    handle: ops
    llm: zulu
    integrations:
      datadog: {enabled: true, role: "dd-role-ops"}
`
}

// A DATADOG SEAT'S SERVER IS SIGNED IN WITH BOTH KEYS, the entry's own API key
// winning over the integration's. Mutation: drop the integration's values from
// config's bindSeatTools, and the server answers 401.
func TestADatadogSeatsServerIsSignedInWithBothKeys(t *testing.T) {
	t.Parallel()
	env := config.NewResolver(config.MapSource{
		"DATADOG_API_KEY": "fake-org-key", "DATADOG_APP_KEY": "fake-provisioner-key",
		"DATADOG_APP_KEY_OPS": "fake-ops-key", "OTHER_ORG_KEY": "fake-other-org-key",
	})
	for _, tc := range []struct {
		name, headers, apiKey string
	}{
		{"the integration's key", "", "fake-org-key"},
		{"the entry's own key", "\n    headers:\n      DD_API_KEY: \"${OTHER_ORG_KEY}\"", "fake-other-org-key"},
	} {
		company := specCompany(t, datadogSeatDoc(tc.headers))
		specs, outcomes := seatSpecs(company, company.Org.AgentSeatByHandle("ops"), env, nil)
		if len(specs) != 1 || len(outcomes) != 0 {
			t.Fatalf("%s: specs = %+v, outcomes = %+v", tc.name, specs, outcomes)
		}
		spec := specs[0]
		spec.URL = twoKeyServer(t, tc.apiKey, "fake-ops-key").URL
		if report := mcp.Test(t.Context(), spec, slog.New(slog.DiscardHandler)); !report.Serving() {
			t.Errorf("%s: the server did not serve with headers %v: %+v", tc.name,
				slices.Sorted(maps.Keys(spec.Headers)), report)
		}
	}

	// WITHOUT THE ORGANIZATION'S KEY the same server refuses the seat.
	company := specCompany(t, datadogSeatDoc(""))
	specs, _ := seatSpecs(company, company.Org.AgentSeatByHandle("ops"), env, nil)
	spec := specs[0]
	delete(spec.Headers, "DD_API_KEY")
	spec.URL = twoKeyServer(t, "fake-org-key", "fake-ops-key").URL
	if mcp.Test(t.Context(), spec, slog.New(slog.DiscardHandler)).Serving() {
		t.Error("the server served a seat that sent only its application key")
	}
}

// AN ORGANIZATION KEY THAT RESOLVES TO NOTHING holds the seat's child back,
// named, and is never offered to the integration's provision command, which
// seals seats' credentials only.
func TestAnUnsetOrganizationKeyIsNotASeatsCredential(t *testing.T) {
	t.Parallel()
	company := specCompany(t, datadogSeatDoc(""))
	env := config.NewResolver(config.MapSource{"DATADOG_APP_KEY_OPS": "fake-ops-key"})
	_, outcomes := seatSpecs(company, company.Org.AgentSeatByHandle("ops"), env, nil)
	var missing *credentialMissing
	if len(outcomes) != 1 || !errors.As(outcomes[0].err, &missing) {
		t.Fatalf("outcomes = %+v, want the child held back", outcomes)
	}
	if reason := missing.reason(); !strings.Contains(reason, "DATADOG_API_KEY") ||
		strings.Contains(reason, "provision command") {
		t.Errorf("the child is held back for %q", reason)
	}
}
