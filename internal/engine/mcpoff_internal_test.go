package engine

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
)

// offCompany declares a shared and a per-seat server, both turned off and both
// reading a reference a test's resolver may or may not answer.
func offCompany(t *testing.T) *Company {
	t.Helper()
	return specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: search
    enabled: false
    command: "search-server"
    env:
      SEARCH_KEY: "${SEARCH_KEY_NOBODY_SET}"
  - name: tracker
    enabled: false
    shared: false
    command: "tracker-server"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      tracker:
        SEAT_TOKEN: "${CEO_TRACKER_TOKEN_NOBODY_SET}"
`)
}

// A SERVER TURNED OFF IS NOBODY'S MISSING CREDENTIAL. Its references are not
// resolved at all, so one that resolves to nothing is not held back, logged or
// reported as waiting for a value: the heartbeat carries no row for either
// server, and nothing reaches a bridge.
func TestAServerTurnedOffIsNotHeldBackForItsCredentials(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := &Engine{}
	e.mcp = mcp.NewBridge(nil, e.sharedSupervision())
	t.Cleanup(func() { _, _ = e.mcp.StopAll(context.Background()) })
	company := offCompany(t)
	e.epoch.current.Store(company)

	e.startSharedServers(ctx, company)
	e.startSeatServers(ctx, "ceo")
	t.Cleanup(func() { e.stopSeatServers(context.Background(), "ceo") })
	if rows := statusOf(ctx, t, e).MCP; len(rows) != 0 {
		t.Errorf("the heartbeat carries %+v for servers turned off, want nothing", rows)
	}
	if running := e.mcp.Servers(); len(running) != 0 {
		t.Errorf("the company bridge was handed %v", running)
	}
	specs, outcomes := seatSpecs(company, company.Org.AgentSeatByHandle("ceo"),
		config.EnvOnly(), nil)
	if len(specs) != 0 || len(outcomes) != 0 {
		t.Errorf("the seat's pass built specs %+v and outcomes %+v for a server turned off", specs, outcomes)
	}
}

// A CODING AGENT'S BOX IS NOT GIVEN A SERVER TURNED OFF, though the seat's box
// block still lists it: the listing is kept for the day the server is turned
// back on. Nothing is withheld for its credentials either, since a server
// nobody runs needs none.
func TestABoxIsNotGivenAServerTurnedOff(t *testing.T) {
	t.Parallel()
	company := offCompany(t)
	seat := company.Org.AgentSeatByHandle("ceo")
	gate := &org.RoleSandbox{MCP: org.RoleSandboxMCP{Servers: []string{"search", "tracker"}}}

	// Nothing resolves: nothing is withheld, since a server turned off needs
	// no credential.
	got, withheld := sandboxMCP(config.EnvOnly(), company, seat, gate, nil)
	if len(got) != 0 || len(withheld) != 0 {
		t.Errorf("the box is given %v and withholds %+v, want neither for servers turned off", got, withheld)
	}
	// Everything resolves: still nothing, so it is the switch that keeps
	// the servers out and not their credentials.
	everyValue := config.NewResolver(config.MapSource{
		"SEARCH_KEY_NOBODY_SET": "search-key", "CEO_TRACKER_TOKEN_NOBODY_SET": "tracker-token",
	})
	if got, withheld := sandboxMCP(everyValue, company, seat, gate, nil); len(got) != 0 || len(withheld) != 0 {
		t.Errorf("with every value resolving the box is given %v and withholds %+v, want neither", got, withheld)
	}
}
