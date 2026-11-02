package config

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

// valuesDoc has a remote server whose key reads a variable of its own, one
// whose key reads a variable another field reads too, a local server with a
// literal, and a per-agent server granted to one seat through its unit.
const valuesDoc = `
name: Acme
mcp_servers:
  - name: linear
    transport: http
    url: "https://mcp.linear.app/mcp"
    headers: {Authorization: "${LINEAR_KEY}"}
  - name: sentry
    transport: http
    url: "https://mcp.sentry.dev/mcp"
    headers: {authorization: "Bearer ${SHARED_TOKEN}", X-Org: "${SHARED_TOKEN}"}
  - {name: notes, command: notes-mcp, env: {NOTES_TOKEN: "plain-not-a-secret"}}
  - {name: tracker, command: tracker-mcp, shared: false}
  - {name: my-x, transport: http, url: "https://x.example.com/mcp"}
  - {name: my, transport: http, url: "https://y.example.com/mcp", headers: {X-Api-Key: "${MCP_MY_X_API_KEY}"}}
roles:
  - name: CEO
    handle: ceo
units:
  - name: Eng
    id: eng
    mcp_env:
      tracker: {TOKEN: "${ENG_TRACKER}"}
    roles:
      - {name: Dev, handle: dev}
      - {name: Lead, handle: lead, kind: human}
`

// A value is sealed under its key's own variable or one of its own, never one
// another field reads. Mutation: reuse a shared variable, and this is red.
func TestAValueIsSealedUnderAVariableOnlyItsKeyReads(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, valuesDoc)
	for _, c := range []struct {
		server, seat, key   string
		wantKey, wantVar    string
		wantRepoint         bool
		wantPath, wantAsked string
	}{
		{"linear", "", "authorization", "Authorization", "LINEAR_KEY", false,
			"mcp_servers[0].headers.Authorization", ""},
		{"sentry", "", "Authorization", "authorization", "MCP_SENTRY_AUTHORIZATION", true,
			"mcp_servers[1].headers.authorization", ""},
		{"sentry", "", "X-Org", "X-Org", "MCP_SENTRY_X_ORG", true, "mcp_servers[1].headers.X-Org", ""},
		{"notes", "", "NOTES_TOKEN", "NOTES_TOKEN", "MCP_NOTES_NOTES_TOKEN", true,
			"mcp_servers[2].env.NOTES_TOKEN", ""},
		{"linear", "", "X-Team", "X-Team", "MCP_LINEAR_X_TEAM", true, "mcp_servers[0].headers.X-Team", ""},
		{"tracker", "dev", "TOKEN", "TOKEN", "MCP_TRACKER_TOKEN_DEV", true,
			"units[0].roles[0].mcp_env.tracker.TOKEN", ""},
	} {
		slot, err := cfg.MCPValueSlot(c.server, c.seat, c.key)
		if err != nil {
			t.Errorf("%s/%s/%s: %v", c.server, c.seat, c.key, err)
			continue
		}
		if slot.Key != c.wantKey || slot.Variable != c.wantVar || slot.Repoint != c.wantRepoint ||
			slot.Path.String() != c.wantPath {
			t.Errorf("%s/%s/%s = key %q var %q repoint %t at %s, want %q %q %t at %s",
				c.server, c.seat, c.key, slot.Key, slot.Variable, slot.Repoint, slot.Path,
				c.wantKey, c.wantVar, c.wantRepoint, c.wantPath)
		}
	}

	_, err := cfg.MCPValueSlot("my-x", "", "Api-Key")
	var shared *MCPValueSharedError
	if !errors.As(err, &shared) || shared.Variable != "MCP_MY_X_API_KEY" ||
		!slices.Equal(shared.Readers, []string{"mcp_servers[5].headers.X-Api-Key"}) {
		t.Errorf("a derived name another field reads = %v, want it refused naming that field", err)
	}
}

// A slot is refused where no value belongs: no server, a seat on a shared
// server, no agent seat, an ungranted seat, a bound key, a bad key.
func TestAValueIsRefusedWhereNoValueBelongs(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, valuesDoc+`
  - name: Infra
    id: infra
    roles:
      - {name: Bot, handle: bot}
`)
	cfg.MCPServers = append(cfg.MCPServers, MCPServer{Name: "github", Transport: TransportHTTP,
		URL: "https://api.githubcopilot.com/mcp/", Shared: org.Off()})
	for _, c := range []struct {
		server, seat, key string
		want              error
	}{
		{"absent", "", "TOKEN", ErrNoMCPServerEntry},
		{"linear", "dev", "Authorization", ErrMCPValueSeatOnShared},
		{"tracker", "nobody", "TOKEN", ErrMCPValueNoSeat},
		{"tracker", "lead", "TOKEN", ErrMCPValueNoSeat},
		{"tracker", "bot", "TOKEN", ErrMCPValueNotGranted},
		{"github", "", "authorization", ErrMCPValueBound},
		{"linear", "", "Bad Key", ErrMCPValueKey},
		{"linear", "", "", ErrMCPValueKey},
	} {
		if _, err := cfg.MCPValueSlot(c.server, c.seat, c.key); !errors.Is(err, c.want) {
			t.Errorf("%s/%s/%q = %v, want %v", c.server, c.seat, c.key, err, c.want)
		}
	}
}

// Pointing a key is the one edit, under the map's own spelling, and the
// document still validates.
func TestPointingAKeyIsTheOneEdit(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, valuesDoc)
	slot, err := cfg.MCPValueSlot("sentry", "", "Authorization")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.PointMCPValue(slot); err != nil {
		t.Fatal(err)
	}
	if got := cfg.MCPServers[1].Headers; len(got) != 2 || got["authorization"] != "${MCP_SENTRY_AUTHORIZATION}" ||
		got["X-Org"] != "${SHARED_TOKEN}" {
		t.Errorf("sentry's headers = %v", got)
	}
	seat, err := cfg.MCPValueSlot("tracker", "dev", "TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.PointMCPValue(seat); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Units[0].Roles[0].MCPEnv["tracker"]; got["TOKEN"] != "${MCP_TRACKER_TOKEN_DEV}" || len(got) != 1 {
		t.Errorf("dev's block = %v", got)
	}
	if got := cfg.Units[0].MCPEnv["tracker"]; got["TOKEN"] != "${ENG_TRACKER}" {
		t.Errorf("the unit's block changed: %v", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the pointed document does not validate: %v", err)
	}
}

// A server's values are listed by name, never by value, a seat's with its
// unit's block laid under its own.
func TestAServersValuesAreListedByName(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, valuesDoc)
	cfg.MCPServers[2].OptionalKeys = []string{"NOTES_TOKEN"}
	sentry, ok := cfg.MCPValueKeys("sentry", "")
	if !ok || len(sentry) != 2 || sentry[1].Key != "X-Org" || sentry[1].Variable != "SHARED_TOKEN" ||
		sentry[0].Key != "authorization" || sentry[0].As != MCPValueHeader ||
		!slices.Equal(sentry[0].Reads, []string{"SHARED_TOKEN"}) {
		t.Errorf("sentry's values = %+v", sentry)
	}
	notes, _ := cfg.MCPValueKeys("notes", "")
	if len(notes) != 1 || notes[0].Variable != "" || len(notes[0].Reads) != 0 || !notes[0].Optional ||
		notes[0].As != MCPValueEnv {
		t.Errorf("notes' values = %+v, want the literal by name alone, optional", notes)
	}
	dev, ok := cfg.MCPValueKeys("tracker", "dev")
	if !ok || len(dev) != 1 || dev[0].Variable != "ENG_TRACKER" {
		t.Errorf("dev's tracker values = %+v, want the unit's block under dev's", dev)
	}
	if _, ok := cfg.MCPValueKeys("tracker", "lead"); ok {
		t.Error("a human seat has values for a tool server")
	}
	if _, ok := cfg.MCPValueKeys("absent", ""); ok {
		t.Error("an undeclared server has values")
	}
}
