package queries_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
)

// serviceCompany has a server the engine signs in to as a service identity,
// and one a person signs in to.
const serviceCompany = `
name: Acme
mcp_servers:
  - name: vendor
    transport: http
    url: https://mcp.vendor.example.com/mcp
    oauth:
      grant: client_credentials
      client_id: crewlet-svc
      client_secret: ${VENDOR_CLIENT_SECRET}
      scopes: [read]
  - name: linear
    transport: http
    url: https://mcp.linear.app/mcp
    oauth:
      client: cimd
`

// A SERVICE IDENTITY IS NEVER A SIGN-IN TO MAKE: its row is decided by its
// instances (a credential its vendor refuses fails them, so the server is
// failing, never `needs_sign_in`), and its view says the grant, the client id
// and the scopes the configuration names, connected, with nobody who signed
// in, no issuer and no registration, whatever grant a person made before; a
// member's reading carries none of it. Never the secret.
//
// Mutation: read the view from the grant record, or leave its status at
// `needs_sign_in`, and the server is drawn as one to sign in to.
func TestAServiceIdentityIsReadAsItsInstancesAndNeverAsASignIn(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte(serviceCompany))
	if err != nil {
		t.Fatal(err)
	}
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{MCP: []coord.MCPServerStatus{
		{Server: "vendor", Shared: true, Failed: 1, ErrorClass: "credential_rejected"},
	}})
	stale := coord.MCPGrant{Server: "vendor", Grant: uuid.New(), Client: "dcr", ClientID: "registered",
		Issuer: "https://auth.vendor.example.com", By: "jane.doe", ByKind: iam.ActorHuman,
		At: oauthNow.Add(-time.Hour)}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{
		Coord: backend, Company: companySource(t, cfg),
		Builtins:  catalogueOffer,
		MCPGrants: grantsReader{grants: []coord.MCPGrant{stale}},
		Now:       func() time.Time { return oauthNow },
	})
	raw, err := r.Answer(everyGrant(t), "mcp_servers_status", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]map[string]any{}
	for _, row := range rowsOf(t, asMap(t, raw)["servers"]) {
		rows[row["name"].(string)] = row
	}
	vendor := rows["vendor"]
	if vendor["state"] != string(queries.MCPFailing) {
		t.Fatalf("the service identity's row is state %v, want its instances' failing", vendor["state"])
	}
	view, _ := vendor["oauth"].(map[string]any)
	if view == nil || view["grant"] != "client_credentials" || view["status"] != "connected" ||
		view["client"] != "" || view["client_id"] != "crewlet-svc" || view["issuer"] != "" ||
		view["connected_by"] != nil || view["expires_at"] != nil || view["failure"] != nil {
		t.Fatalf("the service identity's view is %v", view)
	}
	if scopes, _ := view["scopes"].([]any); len(scopes) != 1 || scopes[0] != "read" {
		t.Fatalf("the scopes are %v, want the configured ones", view["scopes"])
	}
	signIn, _ := rows["linear"]["oauth"].(map[string]any)
	if signIn == nil || signIn["grant"] != "authorization_code" || signIn["status"] != "needs_sign_in" {
		t.Fatalf("a person's sign-in nobody made reads %v", signIn)
	}
	holdShape(t, "McpOAuthView", []map[string]any{view, signIn}, false)
	text, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), "VENDOR_CLIENT_SECRET") {
		t.Fatalf("the answer names the secret's variable: %s", text)
	}

	memberRaw, err := r.Answer(iam.WithPrincipal(t.Context(), person(iam.StageActive, everyAppButAdmin()...)),
		"mcp_servers_status", nil)
	if err != nil {
		t.Fatal(err)
	}
	member, err := json.Marshal(memberRaw)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"client_credentials", "crewlet-svc", `"grant"`} {
		if strings.Contains(string(member), leak) {
			t.Errorf("a member's reading carries %s: %s", leak, member)
		}
	}
}
