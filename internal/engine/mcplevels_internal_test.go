package engine

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// levelledStandIn is an mcp_servers entry for this test binary serving the
// named tools at the levels the `tools` block (YAML flow) sets.
func levelledStandIn(name string, shared bool, listed []string, levels string) string {
	return fmt.Sprintf(`
  - name: %s
    shared: %v
    command: %q
    env:
      %s: "server"
      %s: %q
      %s: %q
    tools: %s
`, name, shared, os.Args[0], ToolServerModeEnv, ToolServerToolEnv, strings.Join(listed, ","),
		procgrouptest.GORACE, procgrouptest.StandInRaceOptions(), levels)
}

// levelledCompany runs a shared server with one tool blocked by name and a
// per-agent one that blocks every tool but one by its default, both reaching
// the one agent seat.
func levelledCompany(t *testing.T) *Company {
	t.Helper()
	return specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:`+
		levelledStandIn("tracker", true, []string{"list_items", "delete_items"},
			"{levels: {delete_items: blocked}}")+
		levelledStandIn("docs", false, []string{"read_doc", "erase_doc"},
			"{default: blocked, levels: {read_doc: allow}}")+`
roles:
  - {name: CEO, handle: ceo, llm: zulu}
`)
}

// A BLOCKED TOOL IS NOT OFFERED, LISTED OR CALLABLE, THROUGH THE RELAY OR THE
// BRIDGE, ON A SHARED SERVER OR A PER-AGENT ONE. The engine hands a server's
// levels to the MCP client with its start, which never files a blocked tool:
// so the seat's surface, which a phase on the engine host serves through its
// tool session (the relay) and an executor's box reaches through the bridge,
// never holds it, however a phase asks for it. Proved end to end, against real
// servers: the shared server's instance and the seat's own child of the
// per-agent one each list a tool the entry blocks and one it allows; a phase's
// surface advertises the allowed two alone; a scripted harness over the real
// tool session has the allowed calls answered by the servers and the blocked
// ones answered by nobody, since its session lists no such tool; and a
// harness's MCP client dialling the bridge, at the job's endpoint and at each
// server's own (mcpbridge.ServerURL), lists the allowed tool of each server
// and no blocked one, and has only the allowed ones answered. The fleet's
// listing names the blocked tools, flagged, for an administrator to let
// through again.
//
// Mutation: drop Levels from serverSpec, or have toolsFor file a blocked tool,
// and every half of this is red.
func TestABlockedToolIsNotOfferedListedOrCallableThroughTheRelayOrTheBridge(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	company := levelledCompany(t)
	e := &Engine{}
	e.mcp = mcp.NewBridge(nil, e.sharedSupervision())
	t.Cleanup(func() { _, _ = e.mcp.StopAll(context.Background()) })
	e.epoch.current.Store(company)
	e.startSharedServers(ctx, company)
	e.startSeatServers(ctx, "ceo")
	t.Cleanup(func() { e.stopSeatServers(context.Background(), "ceo") })

	allowed, blocked := []string{"list_items", "read_doc"}, []string{"delete_items", "erase_doc"}
	asked := slices.Concat(allowed, blocked)
	registry := e.ToolsFor("ceo")
	for _, name := range allowed {
		if _, ok := registry.Lookup(name); !ok {
			t.Fatalf("the seat's registry lacks %s, which its server allows: the servers did not start", name)
		}
	}
	for _, name := range blocked {
		if _, ok := registry.Lookup(name); ok {
			t.Errorf("the seat's registry holds %s, which its server blocks", name)
		}
	}
	if _, ok := e.mcp.Tool("delete_items"); ok {
		t.Error("the shared bridge's catalogue resolves delete_items, which the entry blocks")
	}
	seat := company.Org.AgentSeatByHandle("ceo")
	surfaceOf := func(run string) *tools.Surface {
		return tools.NewSurface("execute", registry.Snapshot(), asked).
			ForTurn(&turnctx.Turn{RunID: run, Seat: seat, Org: company.Org})
	}
	var advertised []string
	for _, def := range surfaceOf("run-defs").ToolDefs() {
		advertised = append(advertised, def.Name)
	}
	if slices.Sort(advertised); !slices.Equal(advertised, allowed) {
		t.Errorf("a phase asking for %v is advertised %v, want only the allowed %v", asked, advertised, allowed)
	}

	t.Run("relay", func(t *testing.T) {
		host, err := toolsession.NewHost()
		if err != nil {
			t.Fatalf("NewHost: %v", err)
		}
		t.Cleanup(func() { _ = host.Close() })
		calls := make([]llmtest.Call, 0, len(asked))
		for _, name := range asked {
			calls = append(calls, llmtest.Call{Name: name})
		}
		script := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
			{Calls: calls}, {Text: "done"},
		}}}}
		_, _ = toolloop.Run(ctx, toolloop.Config{Provider: script, Surface: surfaceOf("run-relay"),
			Host: host, MaxRounds: 2, Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: "you are a seat"}, {Role: llm.RoleUser, Content: "tidy up"},
			}})
		answered := map[string]llmtest.Result{}
		for _, r := range script.Results() {
			answered[r.Name] = r
		}
		for _, name := range allowed {
			if r := answered[name]; r.Failed || r.Output != "ok" {
				t.Errorf("the allowed %s through the relay answered %+v, want the server's ok", name, r)
			}
		}
		// A BLOCKED TOOL IS NO TOOL OF THE SESSION: the harness's client is
		// told the session knows no such tool, and no server ever answers.
		for _, name := range blocked {
			if r := answered[name]; r.Output == "ok" || r.Err == nil ||
				!strings.Contains(r.Err.Error(), "unknown tool") {
				t.Errorf("the blocked %s through the relay answered %+v, want it unknown to the session", name, r)
			}
		}
	})

	t.Run("bridge", func(t *testing.T) {
		mux := http.NewServeMux()
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		bridge, err := mcpbridge.New(mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"),
			BaseURL: server.URL})
		if err != nil {
			t.Fatalf("mcpbridge.New: %v", err)
		}
		mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
		mux.Handle(mcpbridge.ServerPattern, bridge.Handler())
		url := bridge.Open(ctx, &mcpbridge.Session{RunID: "run-bridge", Launch: "job-1", Handle: "ceo",
			Role: "CEO", Surface: surfaceOf("run-bridge")})
		if url == "" {
			t.Fatal("the bridge minted no endpoint")
		}
		if servers := mcpbridge.Servers(surfaceOf("run-servers")); !slices.Equal(servers, []string{"docs", "tracker"}) {
			t.Fatalf("the bridge serves the servers %v, want docs and tracker under their own names", servers)
		}
		dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		dial := func(endpoint string) *sdkmcp.ClientSession {
			client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "harness", Version: "1"}, nil)
			session, err := client.Connect(dialCtx, &sdkmcp.StreamableClientTransport{
				Endpoint: endpoint, HTTPClient: httpxtest.Pool(t)}, nil)
			if err != nil {
				t.Fatalf("dial %s: %v", endpoint, err)
			}
			t.Cleanup(func() { _ = session.Close() })
			return session
		}
		// The job's endpoint serves the engine's own tools, each server
		// its own beneath it: each is listed and called where the harness
		// is handed it.
		for endpoint, want := range map[string][]string{
			url:                                 nil,
			mcpbridge.ServerURL(url, "tracker"): {"list_items"},
			mcpbridge.ServerURL(url, "docs"):    {"read_doc"},
		} {
			session := dial(endpoint)
			listed, err := session.ListTools(dialCtx, nil)
			if err != nil {
				t.Fatalf("tools/list at %s: %v", endpoint, err)
			}
			var names []string
			for _, tool := range listed.Tools {
				if slices.Contains(asked, tool.Name) {
					names = append(names, tool.Name)
				}
			}
			if !slices.Equal(names, want) {
				t.Errorf("%s lists %v of the asked tools, want %v: a blocked tool is listed nowhere", endpoint, names, want)
			}
			for _, name := range asked {
				res, err := session.CallTool(dialCtx, &sdkmcp.CallToolParams{Name: name, Arguments: map[string]any{}})
				answered := err == nil && !res.IsError
				if wanted := slices.Contains(want, name); answered != wanted {
					t.Errorf("tools/call %s at %s: answered %v (err %v, result %+v), want %v",
						name, endpoint, answered, err, res, wanted)
				}
			}
		}
	})

	t.Run("listing", func(t *testing.T) {
		listings := e.nodeMCPListings("node-a", 1, time.Now())
		for server, want := range map[string]map[string]bool{
			"tracker": {"list_items": false, "delete_items": true},
			"docs":    {"read_doc": false, "erase_doc": true},
		} {
			got := map[string]bool{}
			for _, tool := range listings[server].listing.Tools {
				got[tool.Raw] = tool.Blocked
			}
			if !maps.Equal(got, want) {
				t.Errorf("%s's listing flags %v as blocked, want %v: a blocked tool is listed, flagged", server, got, want)
			}
		}
	})
}

// A BOX IS GIVEN NO SERVER THAT HOLDS A TOOL BACK. The harness in a box dials
// a server it is given itself, past the engine, so a level could not hold
// there: a server any tool of which is at a level other than allow is left
// out of the box, whatever the seat's listing says. Admission refuses the
// listing, so a stored revision is what carries one. A server whose levels
// all allow is given as ever.
//
// Mutation: drop the check from sandboxMCP, and this is red.
func TestABoxIsGivenNoServerThatHoldsAToolBack(t *testing.T) {
	t.Parallel()
	company := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - {name: tracker, command: tracker-server, tools: {levels: {delete_items: blocked}}}
  - {name: billing, command: billing-server, tools: {default: blocked}}
  - {name: notes, command: notes-server, tools: {levels: {read_note: allow}}}
roles:
  - {name: CEO, handle: ceo, llm: zulu}
`)
	gate := &org.RoleSandbox{MCP: org.RoleSandboxMCP{Servers: []string{"tracker", "billing", "notes"}}}
	got, withheld := sandboxMCP(config.EnvOnly(), company, company.Org.AgentSeatByHandle("ceo"), gate, nil)
	if names := slices.Sorted(maps.Keys(got)); !slices.Equal(names, []string{"notes"}) || len(withheld) != 0 {
		t.Errorf("the box is given %v (withheld %+v), want only notes, whose every tool is allowed", names, withheld)
	}
}

// A SERVER'S LEVELS REACH ITS SPEC WRITTEN OUT: an entry that says nothing and
// one that says `default: allow` decide every tool alike, so they make one
// spec, and a running server is not restarted for an edit that changes none
// of its tools; a level blocks by the server's own name.
func TestAServersLevelsReachItsSpecWrittenOut(t *testing.T) {
	t.Parallel()
	unset := toolLevels(config.MCPToolPolicy{})
	if explicit := toolLevels(config.MCPToolPolicy{Default: config.MCPToolAllow}); unset.Default != explicit.Default ||
		unset.Default != mcp.LevelAllow || unset.Tools != nil || explicit.Tools != nil {
		t.Fatalf("unset %+v and default allow %+v differ", unset, explicit)
	}
	levels := toolLevels(config.MCPToolPolicy{Default: config.MCPToolBlocked,
		Levels: map[string]config.MCPToolLevel{"read_doc": config.MCPToolAllow}})
	if !levels.Blocks("erase_doc") || levels.Blocks("read_doc") {
		t.Fatalf("levels %+v block erase_doc %v and read_doc %v, want the default to block and the level to allow",
			levels, levels.Blocks("erase_doc"), levels.Blocks("read_doc"))
	}
}
