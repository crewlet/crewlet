package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// A JOB'S OPENING SAYS HOW MANY TOOLS ITS SERVERS LIST: what the bridge opens
// for a job hands its launch the count its session lists across the engine's
// own server and each MCP server's (mcpbridge's Session.Listed), which the
// launch weighs a browser against on a harness that hides every MCP tool
// past a count of them.
//
// Mutation: leave the opening's count unset, and the launch weighs a browser
// against a bridge of no tools.
func TestAJobsOpeningSaysHowManyToolsItsServersList(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	if err := reg.Register(waitedTool{name: "read_page"}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register(read_page): %v", err)
	}
	for _, name := range []string{"create_issue", "search_issues"} {
		if err := reg.Register(waitedTool{name: name}, tools.Origin("github")); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	turn := &turnctx.Turn{RunID: "run-1", Seat: seat, Org: &org.Organization{Name: "Acme", Roles: []*org.Role{seat}}}
	surface := tools.NewSurface("execute", reg.Snapshot(), []string{"read_page", "create_issue", "search_issues"}).
		ForTurn(turn)
	e := &Engine{bridge: bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"),
		BaseURL: "https://engine.example.com"})}
	// MOUNTED, as a node serves it, or the bridge opens no session at all.
	_ = e.bridge.Handler()
	ledger := newBridgeLedger(nil, nil)
	session := &mcpbridge.Session{RunID: "run-1", Handle: "swe", Role: seat.Name, Surface: surface, Ledger: ledger}
	opening, err := e.openBridge(t.Context(), session, ledger, nil, sandbox.OpenedJob{LaunchID: "job-1"})
	if err != nil {
		t.Fatalf("openBridge: %v", err)
	}
	t.Cleanup(func() { e.bridge.Close("run-1") })
	if want := session.Listed(); want != 3 || opening.Tools != want {
		t.Errorf("the opening says its servers list %d tools, want the session's %d of 3", opening.Tools, want)
	}
}

// A LAUNCH COUNTS THE TOOLS OF THE SERVERS ITS BOX IS HANDED: the seat's
// surface holds every tool of a server the box runs, the bridge offering none
// of them, so the launch hands what weighs a browser against a harness that
// hides every MCP tool past a count the tools of each server in the box,
// whole, and none of a server the box is not handed or of the engine's own.
//
// Mutations, each red: leave the count unset; count every MCP tool the
// surface holds; count the surface's active tools in place of its universe.
func TestALaunchCountsTheToolsOfTheServersItsBoxIsHanded(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	if err := reg.Register(waitedTool{name: "read_page"}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register(read_page): %v", err)
	}
	for server, names := range map[string][]string{
		"github": {"create_issue", "search_issues", "get_issue"},
		"linear": {"list_tickets"},
	} {
		for _, name := range names {
			if err := reg.Register(waitedTool{name: name}, tools.Origin(server)); err != nil {
				t.Fatalf("Register(%s): %v", name, err)
			}
		}
	}
	// THE BRIDGE'S ACTIVE LIST LEAVES THE BOX'S SERVER OUT, as an executor's
	// does (internal/agent/runner's executorActive).
	surface := tools.NewSurface("execute", reg.Snapshot(), []string{"read_page", "list_tickets"})
	box := map[string]sandbox.MCPServer{"github": {Name: "github", Transport: sandbox.TransportStdio,
		Command: "github-mcp"}}
	req := runner.AgentRunRequest{Surface: surface}
	if got := withRunsOwn(req, nil, sandbox.LaunchRequest{MCPServers: box}).MCPTools; got != 3 {
		t.Errorf("a launch handing its box github says its servers list %d tools, want github's 3", got)
	}
	if got := withRunsOwn(req, nil, sandbox.LaunchRequest{}).MCPTools; got != 0 {
		t.Errorf("a launch handing its box no server says its servers list %d tools, want none", got)
	}
}
