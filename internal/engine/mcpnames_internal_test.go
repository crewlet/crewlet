package engine

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/tools"
)

// standIn is an mcp_servers entry for this test binary serving one tool.
func standIn(name, tool string, shared bool) string {
	return fmt.Sprintf(`
  - name: %s
    shared: %v
    command: %q
    env:
      %s: "server"
      %s: %q
      %s: %q
`, name, shared, os.Args[0], ToolServerModeEnv, ToolServerToolEnv, tool,
		procgrouptest.GORACE, procgrouptest.StandInRaceOptions())
}

// A TOOL LOST TO ITS NAME IS ON THE HEARTBEAT, with why, on the row of the
// server that lists it: refused where no model can be handed the name (a
// character, the length) or a built-in holds it, and shadowed where another
// server's tool of the name is offered in its place, on one bridge or against
// a seat's own child in that seat's surface. Each was a log line nobody
// reading the server's settings saw; the server that keeps the name says
// nothing, and the tools still offered are offered.
//
// Mutation: skip the surface in unfiled, report a same-server entry as a
// shadow, or let settle drop the counts, and this is red.
func TestAToolLostToItsNameReachesTheHeartbeat(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	long := strings.Repeat("l", harnessprofile.ToolNameMax()+1)
	company := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:`+
		standIn("alpha", "shared_search", true)+
		standIn("beta", "shared_search", true)+
		standIn("colleagues", "lookup_colleague", true)+
		standIn("dotted", "read.file", true)+
		standIn("longnames", long, true)+
		standIn("seatdocs", "shared_search", false)+`
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      seatdocs:
        SEAT_TOKEN: "ceo-token"
`)
	// FILED FIRST, as equip files every built-in before an apply starts a
	// server.
	if err := company.Tools.Register(namedTool("lookup_colleague"), tools.OriginBuiltin); err != nil {
		t.Fatalf("file the built-in: %v", err)
	}
	e := &Engine{}
	e.mcp = mcp.NewBridge(nil, e.sharedSupervision())
	t.Cleanup(func() { _, _ = e.mcp.StopAll(context.Background()) })
	e.epoch.current.Store(company)
	e.startSharedServers(ctx, company)
	e.startSeatServers(ctx, "ceo")
	t.Cleanup(func() { e.stopSeatServers(context.Background(), "ceo") })

	rows := map[string]coord.MCPServerStatus{}
	for _, row := range e.mcpStatus(ctx) {
		rows[row.Server] = row
	}
	check := func(server string, refused []coord.MCPToolRefusal, shadowed []coord.MCPToolShadow, tools int) {
		t.Helper()
		row, ok := rows[server]
		switch {
		case !ok:
			t.Errorf("%s: no heartbeat row", server)
		case row.Started != 1 || row.Tools != tools:
			t.Errorf("%s: started %d serving %d tools, want 1 serving %d: %+v", server, row.Started, row.Tools, tools, row)
		case !slices.Equal(row.Refused, refused) || row.RefusedCount != len(refused):
			t.Errorf("%s: refused %v of %d, want %v", server, row.Refused, row.RefusedCount, refused)
		case !slices.Equal(row.Shadowed, shadowed) || row.ShadowedCount != len(shadowed):
			t.Errorf("%s: shadowed %v of %d, want %v", server, row.Shadowed, row.ShadowedCount, shadowed)
		}
	}
	check("alpha", nil, nil, 1)
	check("beta", nil, []coord.MCPToolShadow{{Tool: "shared_search", By: "alpha"}}, 1)
	check("colleagues", []coord.MCPToolRefusal{{Tool: "lookup_colleague", Reason: string(mcp.NameBuiltin)}}, nil, 1)
	check("dotted", []coord.MCPToolRefusal{{Tool: "read.file", Reason: string(mcp.NameCharacters)}}, nil, 0)
	check("longnames", []coord.MCPToolRefusal{{Tool: long, Reason: string(mcp.NameTooLong)}}, nil, 0)
	check("seatdocs", nil, []coord.MCPToolShadow{{Tool: "shared_search", By: "alpha"}}, 1)

	surface := e.ToolsFor("ceo")
	if entry, ok := surface.Lookup("shared_search"); !ok || entry.Origin != mcp.Origin("alpha") {
		t.Errorf("the seat is offered shared_search from %q, want alpha's", entry.Origin)
	}
	if entry, ok := surface.Lookup("lookup_colleague"); !ok || entry.Origin != tools.OriginBuiltin {
		t.Errorf("the seat's lookup_colleague is %q's, want the built-in", entry.Origin)
	}
	for _, name := range []string{"read.file", long, long[:harnessprofile.ToolNameMax()]} {
		if _, ok := surface.Lookup(name); ok {
			t.Errorf("the seat is offered %q, a name no model can be handed", name)
		}
	}

	// A NAME THE SURFACE HOLDS FOR THE SAME SERVER is that server's own
	// tool not refiled yet (a restart's new object before the refile), not
	// a collision: alpha is not shadowed by alpha.
	company.Tools.Unregister("shared_search")
	if err := company.Tools.Register(namedTool("shared_search"), mcp.Origin("alpha")); err != nil {
		t.Fatalf("file a stale alpha tool: %v", err)
	}
	for _, row := range e.mcpStatus(ctx) {
		if row.Server == "alpha" && (row.ShadowedCount != 0 || row.RefusedCount != 0) {
			t.Errorf("alpha's own tool not refiled yet reads as %+v", row)
		}
	}
}

// AN ADDRESS THE RULE REFUSES IS NOT DIALLED, and the box is not given it: an
// address written with a ${VAR} passed the document, and is judged once it
// resolves, by the rule the document is held to. Said without the address,
// which can carry a secret once resolved.
//
// Mutation: drop the check from serverSpec or from sandboxMCP, and this is
// red.
func TestAnAddressTheRuleRefusesIsNotDialledOrBoxed(t *testing.T) {
	t.Parallel()
	company := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: docs
    transport: http
    url: "${DOCS_URL}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`)
	server := &company.Config.MCPServers[0]
	seat := company.Org.AgentSeatByHandle("ceo")
	gate := &org.RoleSandbox{MCP: org.RoleSandboxMCP{Servers: []string{"docs"}}}
	for _, tc := range []struct {
		url    string
		dialed bool
	}{
		{"https://docs.example.com/mcp", true},
		{"http://docs.internal:8080/mcp", true},
		{"http://docs.example.com/mcp", false},
		{"https://reader:hunter2-secret@docs.example.com/mcp", false},
	} {
		env := config.NewResolver(config.MapSource{"DOCS_URL": tc.url})
		values, missing := resolveServer(server, "", "docs", nil, env, nil)
		if missing != nil {
			t.Fatalf("%s: held back for %v", tc.url, missing)
		}
		_, err := serverSpec(server, "docs", values)
		if (err == nil) != tc.dialed {
			t.Errorf("%s: serverSpec err = %v, want dialled %v", tc.url, err, tc.dialed)
		}
		if err != nil && (strings.Contains(err.Error(), "docs.example.com") || strings.Contains(err.Error(), "hunter2")) {
			t.Errorf("%s: the refusal quotes the address: %v", tc.url, err)
		}
		boxed, _ := sandboxMCP(env, company, seat, gate, nil)
		if _, given := boxed["docs"]; given != tc.dialed {
			t.Errorf("%s: the box is given the server: %v, want %v", tc.url, given, tc.dialed)
		}
	}
}
