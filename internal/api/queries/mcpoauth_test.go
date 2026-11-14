package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
)

// oauthCompany has four remote servers signed in to with OAuth, each in a
// different place, and one that is not.
const oauthCompany = `
name: Acme
mcp_servers:
  - name: linear
    transport: http
    url: https://mcp.linear.app/mcp
    oauth:
      client: cimd
  - name: notion
    transport: http
    url: https://mcp.notion.com/mcp
    oauth:
      client: dcr
      scopes: [read]
  - name: sentry
    transport: http
    url: https://mcp.sentry.dev/mcp
    oauth:
      client: own
      client_id: crewlet-acme
  - name: stripe
    transport: http
    url: https://mcp.stripe.com
    oauth:
      client: dcr
  - name: moved
    transport: http
    url: https://mcp.moved.example.com/mcp
    oauth:
      client: cimd
  - name: docs
    command: docs-mcp
`

// grantsReader answers the grants a case wrote, or err.
type grantsReader struct {
	grants []coord.MCPGrant
	err    error
}

func (g grantsReader) MCPGrants(context.Context) ([]coord.MCPGrant, error) { return g.grants, g.err }

func (grantsReader) MCPSignInAddresses() (string, string) {
	return "https://crewlet.example.com/webhooks/mcp-oauth",
		"https://crewlet.example.com/webhooks/mcp-oauth/client-metadata.json"
}

var oauthNow = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// oauthSources is the OAuth company, every node reporting, and its grants:
// linear connected and running out in a week, notion refused by its vendor,
// stripe past its stated end, and nobody signed in to sentry.
func oauthSources(t *testing.T) queries.Sources {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(oauthCompany))
	if err != nil {
		t.Fatal(err)
	}
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{MCP: []coord.MCPServerStatus{
		{Server: "linear", Shared: true, Started: 1, Tools: 12},
		{Server: "notion", Shared: true, Failed: 1, ErrorClass: "needs_sign_in"},
		{Server: "moved", Shared: true, Failed: 1, ErrorClass: "needs_sign_in",
			Error: "moved is not started: its sign-in was made for another address"},
	}})
	week, failedAt, past := oauthNow.Add(7*24*time.Hour), oauthNow.Add(-time.Hour), oauthNow.Add(-time.Minute)
	grant := func(server string) coord.MCPGrant {
		return coord.MCPGrant{Server: server, Grant: uuid.New(), Client: "cimd",
			ClientID: "https://crewlet.example.com/webhooks/mcp-oauth/client-metadata.json",
			Issuer:   "https://auth.example.com", Scopes: []string{"read", "offline_access"},
			Resource: "https://mcp.example.com/mcp",
			By:       "jane.doe", ByKind: iam.ActorHuman, At: oauthNow.Add(-24 * time.Hour)}
	}
	linear, notion, stripe, moved := grant("linear"), grant("notion"), grant("stripe"), grant("moved")
	linear.Expiry = &week
	notion.Failure, notion.FailedAt = "the vendor refused to renew the sign-in: invalid_grant", &failedAt
	stripe.Expiry = &past
	return queries.Sources{
		Coord: backend, Company: companySource(t, cfg),
		Builtins:  catalogueOffer,
		MCPGrants: grantsReader{grants: []coord.MCPGrant{linear, moved, notion, stripe}},
		Now:       func() time.Time { return oauthNow },
	}
}

// A SERVER SIGNED IN TO WITH OAUTH IS READ AS ITS GRANT SAYS: connected and
// running where a grant renews; `needs_sign_in`, whatever its nodes report,
// where nobody holds one, its vendor refused to renew it or it ran out; the
// grant's public half on the administrator's reading, with the vendor's words
// for a refusal; the two addresses a vendor is given; and nothing of any of it
// on a member's reading but the state. Never a token.
//
// Mutation: decide the state from the counts alone, or answer the view to a
// member, and this is red.
func TestAServerSignedInToWithOAuthIsReadAsItsGrantSays(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, oauthSources(t))
	raw, err := r.Answer(everyGrant(t), "mcp_servers_status", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := asMap(t, raw)
	if body["callback_url"] != "https://crewlet.example.com/webhooks/mcp-oauth" ||
		body["client_metadata_url"] != "https://crewlet.example.com/webhooks/mcp-oauth/client-metadata.json" {
		t.Errorf("the addresses are %v and %v", body["callback_url"], body["client_metadata_url"])
	}
	rows := map[string]map[string]any{}
	for _, row := range rowsOf(t, body["servers"]) {
		rows[row["name"].(string)] = row
	}
	for server, want := range map[string]struct{ state, oauth string }{
		"linear": {"running", "connected"},
		"notion": {"needs_sign_in", "refresh_failed"},
		"stripe": {"needs_sign_in", "expired"},
		"sentry": {"needs_sign_in", "needs_sign_in"},
		// HELD BACK BY THE NODES, its grant made for another address.
		"moved": {"needs_sign_in", "connected"},
	} {
		row := rows[server]
		view, _ := row["oauth"].(map[string]any)
		if row["state"] != want.state || view == nil || view["status"] != want.oauth {
			t.Errorf("%s = state %v, oauth %v; want %s and %s", server, row["state"], row["oauth"],
				want.state, want.oauth)
		}
	}
	if rows["docs"]["oauth"] != nil {
		t.Errorf("a server not signed in to with OAuth carries %v", rows["docs"]["oauth"])
	}
	views := []map[string]any{}
	for _, server := range []string{"linear", "notion", "stripe", "sentry", "moved"} {
		views = append(views, rows[server]["oauth"].(map[string]any))
	}
	holdShape(t, "McpOAuthView", views, false)
	failure, _ := rows["notion"]["oauth"].(map[string]any)["failure"].(map[string]any)
	if failure == nil || !strings.Contains(failure["said"].(string), "invalid_grant") {
		t.Fatalf("notion's failure is %v, want the vendor's words", failure)
	}
	holdShape(t, "McpOAuthFailure", []map[string]any{failure}, false)
	sentry := rows["sentry"]["oauth"].(map[string]any)
	if sentry["client_id"] != "crewlet-acme" || sentry["connected_by"] != nil {
		t.Errorf("sentry, nobody signed in, reads %v", sentry)
	}
	got, err := clientsource.Union(clientsource.Tree(t), "McpOAuthStatus")
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the client's declaration", err)
	}
	if want := stringsOf(queries.MCPOAuthStatuses); !slices.Equal(slices.Sorted(slices.Values(got)),
		slices.Sorted(slices.Values(want))) {
		t.Errorf("the dashboard's McpOAuthStatus is %v; the engine sends %v", got, want)
	}

	memberRaw, err := r.Answer(iam.WithPrincipal(t.Context(), person(iam.StageActive, everyAppButAdmin()...)),
		"mcp_servers_status", nil)
	if err != nil {
		t.Fatal(err)
	}
	text, err := json.Marshal(memberRaw)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{`"oauth"`, "callback_url", "client_metadata_url", "jane.doe",
		"auth.example.com", "invalid_grant", "crewlet-acme"} {
		if strings.Contains(string(text), leak) {
			t.Errorf("a member's reading carries %s: %s", leak, text)
		}
	}
	if !strings.Contains(string(text), `"needs_sign_in"`) {
		t.Errorf("a member's reading does not say which server needs a sign-in: %s", text)
	}
}

// A GRANT THAT CANNOT BE READ IS UNKNOWN, never "nobody signed in": the answer
// fails rather than tell somebody to sign in again over a store blip.
func TestAnUnreadableGrantIsNoSignOut(t *testing.T) {
	t.Parallel()
	src := oauthSources(t)
	src.MCPGrants = grantsReader{err: errors.New("the coordination store did not answer")}
	r := queries.NewRegistry()
	queries.Register(r, src)
	if _, err := r.Answer(everyGrant(t), "mcp_servers_status", nil); err == nil {
		t.Fatal("an unreadable grant answered the servers")
	}
}
