package engine

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// limitedCompany runs a shared server whose entry blocks wipe_items and holds
// close_items at approve, and a per-agent one, both reaching every agent seat.
// The ceo sits at the root, held to nothing. The eng unit holds list_items at
// approve, close_items at approve (its entry's own level) and wipe_items at
// approve (which its entry blocks), and erase_doc at blocked; its dev holds
// read_doc at approve on its own; and its ops turns the shared server off.
func limitedCompany(t *testing.T) *Company {
	t.Helper()
	return specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:`+
		levelledStandIn("tracker", true, []string{"list_items", "close_items", "wipe_items"},
			"{default: allow, levels: {wipe_items: blocked, close_items: approve}}")+
		levelledStandIn("docs", false, []string{"read_doc", "erase_doc"}, "{default: allow}")+`
roles:
  - {name: CEO, handle: ceo, llm: zulu}
units:
  - name: Eng
    id: eng
    mcp_limits:
      tracker: {tools: {list_items: approve, close_items: approve, wipe_items: approve}}
      docs: {tools: {erase_doc: blocked}}
    roles:
      - name: Dev
        handle: dev
        llm: zulu
        mcp_limits:
          docs: {tools: {read_doc: approve}}
      - name: Ops
        handle: ops
        llm: zulu
        mcp_limits:
          tracker: {off: true}
`)
}

// A SEAT'S LIMITS HOLD EVERY SURFACE IT IS OFFERED TOOLS ON TO THE STRICTER OF
// ITS ENTRY AND ITS LIMITS, and never loosen one. Proved against real servers,
// a shared one and each seat's own child of a per-agent one: a tool a limit
// blocks is on no surface of the seat, a tool it holds at approve waits for a
// person's answer, a tool the entry blocks stays off though a limit holds it
// only at approve (a limit cannot bring back what an administrator blocked),
// a tool the entry holds at approve keeps waiting, and a server a limit turns
// off offers the seat nothing. The seat at the root, held to nothing, is
// offered each tool at its entry's own level, and the epoch's registry keeps
// it there: a seat's cut is its own copy. The executor's box is given neither
// server for a seat whose limits hold one of its tools back.
//
// Mutation: drop limitSurface from grantedSurface or seatSurface, let a limit
// at approve re-file a blocked tool, or drop the limits from sandboxMCP, and
// this is red.
func TestASeatsLimitsHoldItsSurfaceToTheStricterOfEntryAndLimit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	company := limitedCompany(t)
	e := &Engine{}
	e.mcp = mcp.NewBridge(nil, e.sharedSupervision())
	t.Cleanup(func() { _, _ = e.mcp.StopAll(context.Background()) })
	e.epoch.current.Store(company)
	e.startSharedServers(ctx, company)
	for _, handle := range []string{"ceo", "dev", "ops"} {
		e.startSeatServers(ctx, handle)
		t.Cleanup(func() { e.stopSeatServers(context.Background(), handle) })
	}

	// offered is each MCP tool on the seat's surface and whether it waits
	// for a person's answer.
	offered := func(reg *tools.Registry) map[string]bool {
		out := map[string]bool{}
		for _, entry := range reg.List() {
			if _, fromMCP := entry.FromMCP(); fromMCP {
				out[entry.Name()] = entry.NeedsApproval
			}
		}
		return out
	}
	for handle, want := range map[string]map[string]bool{
		"ceo": {"list_items": false, "close_items": true, "read_doc": false, "erase_doc": false},
		"dev": {"list_items": true, "close_items": true, "read_doc": true},
		"ops": {"read_doc": false},
	} {
		got := offered(e.ToolsFor(handle))
		if !maps.Equal(got, want) {
			t.Errorf("%s is offered %v, want %v (true waits for a person)", handle, got, want)
		}
		// THE FALLBACK FOR A SEAT THIS NODE HOLDS NO CHILDREN FOR is cut
		// the same way from the epoch's shared registry.
		shared := offered(grantedSurface(company, company.Org.AgentSeatByHandle(handle)))
		for name, waits := range shared {
			if wanted, ok := want[name]; !ok || wanted != waits {
				t.Errorf("%s's fallback surface offers %s (waits %v), want %v", handle, name, waits, want)
			}
		}
	}
	if entry, ok := company.Tools.Lookup("list_items"); !ok || entry.NeedsApproval {
		t.Error("the epoch's registry holds list_items at approve: a seat's limits reached every seat")
	}

	// A HELD SEAT'S SURFACE, through the tool loop's own frame: the tool a
	// limit holds at approve is handed to the holder, not made.
	seat := company.Org.AgentSeatByHandle("dev")
	hold := &recordingHold{}
	surface := tools.NewSurface("execute", e.ToolsFor("dev").Snapshot(), []string{"list_items", "erase_doc"}).
		ForTurn(&turnctx.Turn{RunID: "run-1", Seat: seat, Org: company.Org}).WithHolder(hold)
	var advertised []string
	for _, def := range surface.ToolDefs() {
		advertised = append(advertised, def.Name)
	}
	if !slices.Equal(advertised, []string{"list_items"}) {
		t.Errorf("dev's executor is advertised %v, want list_items alone: erase_doc is blocked for it", advertised)
	}
	res, err := surface.Execute(ctx, llm.ToolCall{Name: "list_items", Arguments: map[string]any{"q": "open"}})
	if err != nil || res.Output == "ok" || len(hold.held) != 1 || hold.held[0].Args["q"] != "open" {
		t.Errorf("dev's call to list_items answered %+v, %v, held %+v: want it held for a person, not made",
			res, err, hold.held)
	}

	gate := &org.RoleSandbox{MCP: org.RoleSandboxMCP{Servers: []string{"docs"}}}
	for handle, want := range map[string][]string{"ceo": {"docs"}, "dev": nil, "ops": nil} {
		got, withheld := sandboxMCP(config.EnvOnly(), company, company.Org.AgentSeatByHandle(handle), gate, nil)
		if names := slices.Sorted(maps.Keys(got)); !slices.Equal(names, want) || len(withheld) != 0 {
			t.Errorf("%s's box is given %v (withheld %+v), want %v", handle, names, withheld, want)
		}
	}
}

// A LIMIT ONLY EVER ADDS THE WAIT: RequireApproval marks an MCP tool, leaves a
// builtin, which no level governs, as it is, and reports a tool not filed.
func TestRequireApprovalOnlyMarksAFiledMCPTool(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	if err := reg.Register(namedTool("lookup_colleague"), tools.OriginBuiltin); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(namedTool("search_web"), tools.Origin("search")); err != nil {
		t.Fatal(err)
	}
	if reg.RequireApproval("lookup_colleague") || reg.RequireApproval("nothing") {
		t.Error("RequireApproval marked a builtin or a tool not filed")
	}
	if entry, _ := reg.Lookup("lookup_colleague"); entry.NeedsApproval {
		t.Error("a builtin waits for a person")
	}
	if !reg.RequireApproval("search_web") {
		t.Fatal("RequireApproval did not mark an MCP tool")
	}
	if entry, _ := reg.Lookup("search_web"); !entry.NeedsApproval {
		t.Error("the MCP tool does not wait for a person")
	}
}
