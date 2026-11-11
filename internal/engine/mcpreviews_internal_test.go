package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcpreview"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// changingStandIn is a shared stand-in server listing one tool, close_items,
// allowed by its entry, whose description it reads from the file describe at
// each listing and whose listing it says stays fresh for 100ms: a server that
// can change its tool while it runs.
func changingStandIn(name, describe string) string {
	return fmt.Sprintf(`
  - name: %s
    shared: true
    command: %q
    env:
      %s: "server"
      %s: "close_items"
      %s: %q
      %s: "100"
      %s: %q
    tools: {levels: {close_items: allow}}
`, name, os.Args[0], ToolServerModeEnv, ToolServerToolEnv, ToolServerDescribeEnv, describe,
		ToolServerTTLEnv, procgrouptest.GORACE, procgrouptest.StandInRaceOptions())
}

// reviewingEngine is an engine over a fleet store with the company running and
// its watch of the accepted definitions answered, its sessions listed again a
// floor of 50ms after each listing.
func reviewingEngine(t *testing.T, company *Company) (*Engine, *coordmemory.Fleet) {
	t.Helper()
	fleet := coordmemory.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet}, testNode: "node-a", mcpRelistBase: 50 * time.Millisecond}
	e.mcp = mcp.NewBridge(nil, e.sharedSupervision())
	t.Cleanup(func() { _, _ = e.mcp.StopAll(context.Background()) })
	e.epoch.current.Store(company)
	watchCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.watchMCPAccepted(watchCtx) }()
	t.Cleanup(func() { stop(); <-done })
	waitFor(t, "the accepted definitions' watch to answer", func() bool {
		_, answered := e.reviewIndex()
		return answered
	})
	return e, fleet
}

// heldOnce is a [tools.Holder] recording what it is handed.
type heldOnce struct{ held []tools.HeldCall }

func (h *heldOnce) Hold(_ context.Context, _ *turnctx.Turn, call tools.HeldCall) (tools.Result, bool) {
	h.held = append(h.held, call)
	return tools.Result{Output: "held for a person"}, true
}

// A CHANGED DEFINITION HOLDS AN ALLOWED TOOL AT APPROVE UNTIL ACCEPTED (ADR-0078,
// the series' named invariant), end to end against a real server that changes
// its tool while it runs. Its first listing is accepted as it is, and the tool
// its entry allows runs unasked; the server rewords the tool, the engine lists
// it again on the TTL it states and refiles it, and from then on a phase on the
// engine host is refused the call, saying the definition changed, and the
// executor's run is asked to park on it, marked as held for a change; once a
// person accepts the new definition the call runs unasked again, on every
// surface, with nothing refiled.
//
// Mutation: drop Entry.Unreviewed from Registry.RegisterMCP, decide
// toolUnreviewed by the server alone, or accept the listing again at every
// beat, and this is red.
func TestAChangedDefinitionHoldsAnAllowedToolAtApproveUntilAccepted(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	describe := filepath.Join(t.TempDir(), "describe")
	if err := os.WriteFile(describe, []byte("Close the items"), 0o600); err != nil {
		t.Fatal(err)
	}
	company := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:`+changingStandIn("tracker", describe)+`
roles:
  - {name: CEO, handle: ceo, llm: zulu}
`)
	e, fleet := reviewingEngine(t, company)
	e.startSharedServers(ctx, company)
	e.baselineMCPTools(ctx, e.nodeMCPListings("node-a", 1, time.Now()), time.Now())
	waitFor(t, "the first listing to be accepted", func() bool {
		index, _ := e.reviewIndex()
		return index.Baselined("tracker")
	})

	seat := company.Org.AgentSeatByHandle("ceo")
	turn := &turnctx.Turn{RunID: "run-1", Seat: seat, Org: company.Org}
	call := func(phase string, hold tools.Holder) (string, bool) {
		surface := tools.NewSurface(phase, e.ToolsFor("ceo").Snapshot(), []string{"close_items"}).ForTurn(turn)
		if hold != nil {
			surface = surface.WithHolder(hold)
		}
		res, err := surface.Execute(ctx, llm.ToolCall{Name: "close_items", Arguments: map[string]any{}})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return res.Output, res.Held
	}
	if out, held := call("review", nil); out != "ok" || held {
		t.Fatalf("the accepted tool answered %q (held %v), want it to run unasked", out, held)
	}
	entry, _ := e.ToolsFor("ceo").Lookup("close_items")
	first := entry.Tool.(*mcp.Tool).Digest()

	if err := os.WriteFile(describe, []byte("Close the items, and delete them"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the changed tool to be refiled", func() bool {
		entry, ok := e.ToolsFor("ceo").Lookup("close_items")
		return ok && entry.Tool.(*mcp.Tool).Digest() != first
	})
	if out, _ := call("review", nil); !strings.Contains(out, "definition changed") {
		t.Errorf("a host phase's call to the changed tool answered %q, want it refused for the change", out)
	}
	hold := &heldOnce{}
	if _, held := call("execute", hold); !held || len(hold.held) != 1 || !hold.held[0].Changed {
		t.Fatalf("the executor's call was held %v as %+v, want it held for the change", held, hold.held)
	}

	// A PERSON ACCEPTS THE DEFINITION LISTED NOW.
	listed := e.nodeMCPListings("node-a", 1, time.Now())["tracker"].listing.Tools[0]
	stored, err := fleet.ListMCPAccepted(ctx)
	if err != nil || len(stored) != 1 {
		t.Fatalf("accepted = %+v (%v), want the first listing's one tool", stored, err)
	}
	if _, wrote, err := fleet.PutMCPAccepted(ctx, mcpreview.Accepting("tracker", listed, "ana", time.Now()),
		stored[0].Version); err != nil || !wrote {
		t.Fatalf("accepting the change = (%v, %v)", wrote, err)
	}
	waitFor(t, "the accepted change to release the tool", func() bool {
		out, held := call("execute", &heldOnce{})
		return out == "ok" && !held
	})
}

// A SERVER'S FIRST LISTING IS ACCEPTED ONCE, by whichever node lists it first:
// a second node's beat accepts nothing more, so a tool the server adds after
// its first listing waits for a person whoever lists it.
//
// Mutation: baseline from the watched copy alone, or put each record whatever
// is held, and this is red.
func TestAServersFirstListingIsAcceptedOnceAndAnAdditionAfterItIsNot(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, fleet := reviewingEngine(t, specCompany(t, "name: Acme\n"))
	e.baselineMCPTools(ctx, map[string]nodeListing{"github": fakeListing("github", "node-a", 1, "get_issue")},
		time.Now())
	waitFor(t, "the first listing to be accepted", func() bool {
		index, _ := e.reviewIndex()
		return index.Baselined("github")
	})
	// A PEER ONE STEP BEHIND, whose watched copy has not heard of the
	// baseline: it reads the store before it accepts anything.
	peer, _ := reviewingEngineOver(t, fleet)
	peer.reviews.mu.Lock()
	peer.reviews.index = mcpreview.Index{}
	peer.reviews.mu.Unlock()
	peer.baselineMCPTools(ctx, map[string]nodeListing{
		"github": fakeListing("github", "node-b", 1, "get_issue", "delete_repo")}, time.Now())
	held, err := fleet.ListMCPAccepted(ctx)
	if err != nil || len(held) != 1 || held[0].Tool != "get_issue" || !held[0].First {
		t.Fatalf("accepted = %+v (%v), want get_issue alone, from the first listing", held, err)
	}
	if !mcpreview.IndexOf(held).Unreviewed("github", "delete_repo", testDigest("github/delete_repo")) {
		t.Error("a tool added after the first listing runs unasked")
	}
}

// reviewingEngineOver is a second node's engine over the same fleet store, its
// watch answered.
func reviewingEngineOver(t *testing.T, fleet *coordmemory.Fleet) (*Engine, *coordmemory.Fleet) {
	t.Helper()
	e := &Engine{backends: &Backends{Fleet: fleet}, testNode: "node-b"}
	e.epoch.current.Store(specCompany(t, "name: Acme\n"))
	watchCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.watchMCPAccepted(watchCtx) }()
	t.Cleanup(func() { stop(); <-done })
	waitFor(t, "the peer's watch to answer", func() bool {
		_, answered := e.reviewIndex()
		return answered
	})
	return e, fleet
}

// UNANSWERED IS UNREVIEWED: once this node watches what was accepted, and
// until it has read it, every tool asks and no box is given a server; a node
// that does not watch (no coordination store) decides nothing.
func TestANodeThatHasNotReadWhatWasAcceptedHoldsEveryTool(t *testing.T) {
	t.Parallel()
	e := &Engine{backends: &Backends{Fleet: coordmemory.NewFleet()}}
	if e.toolUnreviewed("github", "get_issue", testDigest("github/get_issue")) {
		t.Error("a node that does not watch the accepted definitions holds a tool")
	}
	e.reviews.watching = true
	if !e.toolUnreviewed("github", "get_issue", testDigest("github/get_issue")) {
		t.Error("a node that has not read the accepted definitions runs a tool unasked")
	}
	if held, _ := e.boxHoldsChange("github"); !held {
		t.Error("a node that has not read the accepted definitions gives a box a server")
	}
	if (&Engine{}).unreviewedDecider() != nil {
		t.Error("a node with no fleet store holds tools for changes it cannot read")
	}
}

// WHAT WAS ACCEPTED KEEPS ITS DEFINITION: a change is shown beside the
// definition its level was decided on, which no listing names once the server
// changed it, so the sweep keeps every definition an accepted record names; and an
// accepted definition of a server nothing declares or lists goes, so a server
// added later under its name lists for the first time.
//
// Mutation: compute the sweep's named digests from the listings alone, or keep
// every accepted definition, and this is red.
func TestTheSweepKeepsTheDefinitionWhatWasAcceptedNames(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmemory.NewFleet()
	backend := coordmemory.New()
	e := &Engine{backends: &Backends{Fleet: fleet, Coord: backend}}
	e.epoch.current.Store(specCompany(t, `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp}
`))
	old := testDigest("tracker/close_items@1")
	if _, err := fleet.CreateMCPToolDefinition(ctx, coord.MCPToolDefinition{Digest: old,
		Input: []byte(`{"type":"object"}`), At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	accept := func(server, tool, digest string) {
		t.Helper()
		part := digest[:coord.MCPToolPartLength]
		a := coord.MCPAcceptedTool{Server: server, Tool: tool, Digest: digest, First: true, At: time.Now(),
			Hints: coord.MCPToolHints{ReadOnly: "yes", Destructive: "no", Idempotent: "no", OpenWorld: "no"},
			Parts: coord.MCPToolParts{Title: part, Description: part, Schema: part, Hints: part}}
		if _, wrote, err := fleet.PutMCPAccepted(ctx, a, 0); err != nil || !wrote {
			t.Fatalf("PutMCPAccepted(%s/%s) = (%v, %v)", server, tool, wrote, err)
		}
	}
	accept("tracker", "close_items", old)
	accept("gone", "search", testDigest("gone/search"))
	for pass := 1; pass <= mcpListingSweepPasses+1; pass++ {
		if _, err := e.sweepMCPListings(ctx); err != nil {
			t.Fatalf("sweep %d: %v", pass, err)
		}
	}
	if _, found, err := fleet.MCPToolDefinition(ctx, old); err != nil || !found {
		t.Errorf("the schema the accepted definition names was swept (%v)", err)
	}
	held, err := fleet.ListMCPAccepted(ctx)
	if err != nil || len(held) != 1 || held[0].Server != "tracker" {
		t.Errorf("accepted after the sweep = %+v (%v), want tracker's alone", held, err)
	}
}

// A BOX IS GIVEN NO SERVER A TOOL OF WHICH CHANGED: it reaches a server whole,
// so a call it made could not be held for a person; the seat reaches the
// server's tools over the bridge instead. A server whose tools are as accepted
// is given.
func TestABoxIsGivenNoServerAToolOfWhichChanged(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, _ := reviewingEngine(t, specCompany(t, "name: Acme\n"))
	accepted := fakeListing("tracker", "node-a", 1, "close_items")
	changed := fakeListing("tracker", "node-a", 1, "close_items", "purge_items")
	e.baselineMCPTools(ctx, map[string]nodeListing{"tracker": accepted,
		"docs": fakeListing("docs", "node-a", 1, "search")}, time.Now())
	waitFor(t, "the first listings to be accepted", func() bool {
		index, _ := e.reviewIndex()
		return index.Baselined("tracker") && index.Baselined("docs")
	})
	e.listings.mu.Lock()
	e.listings.answered, e.listings.presenceRead = true, true
	e.listings.presence = []coord.Lease{{Resource: coord.NodeResource("node-a"), Epoch: 1}}
	e.listings.fleet = map[string]coord.MCPListing{
		coord.MCPListingKey("tracker", "node-a"): changed.listing,
		coord.MCPListingKey("docs", "node-a"):    fakeListing("docs", "node-a", 1, "search").listing,
	}
	e.listings.mu.Unlock()
	servers := e.withoutChangedServers(ctx, "ceo", map[string]sandbox.MCPServer{
		"tracker": {Name: "tracker"}, "docs": {Name: "docs"}})
	if _, given := servers["tracker"]; given {
		t.Error("a box was given a server a tool of which the server added since its first listing")
	}
	if _, given := servers["docs"]; !given {
		t.Error("a box was refused a server whose tools are as accepted")
	}
}

// A CHANGED TOOL STANDS ON ITS SERVER'S CONDITION, counted where the change
// holds it (its level would run it unasked) and dated by when the fleet first
// stored its definition; a change to a tool that asks anyway, and a removal,
// count for nothing.
func TestAChangedToolStandsOnItsServersCondition(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	company := specCompany(t, `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, tools: {levels: {purge_items: approve}}}
`)
	e, fleet := reviewingEngine(t, company)
	e.baselineMCPTools(ctx, map[string]nodeListing{
		"tracker": fakeListing("tracker", "node-a", 1, "close_items", "purge_items", "old_tool")}, time.Now())
	waitFor(t, "the first listing to be accepted", func() bool {
		index, _ := e.reviewIndex()
		return index.Baselined("tracker")
	})
	now := fakeListing("tracker", "node-a", 1, "close_items", "purge_items", "new_tool")
	for i := range now.listing.Tools {
		if now.listing.Tools[i].Raw == "purge_items" {
			now.listing.Tools[i].Digest = testDigest("tracker/purge_items@2")
		}
	}
	seen := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	if _, err := fleet.CreateMCPToolDefinition(ctx, coord.MCPToolDefinition{Digest: testDigest("tracker/new_tool"),
		Input: []byte(`{"type":"object"}`), At: seen}); err != nil {
		t.Fatal(err)
	}
	e.listings.mu.Lock()
	e.listings.answered, e.listings.presenceRead = true, true
	e.listings.presence = []coord.Lease{{Resource: coord.NodeResource("node-a"), Epoch: 1}}
	e.listings.fleet = map[string]coord.MCPListing{coord.MCPListingKey("tracker", "node-a"): now.listing}
	e.listings.mu.Unlock()
	conds := []statelog.MCPServerCondition{{Server: "tracker"}}
	e.changedConditions(ctx, company, conds)
	got := conds[0]
	if got.Changes != 1 || got.Change.Tool != "new_tool" || got.Change.Kind != "added" ||
		!got.ChangeSince.Equal(seen) {
		t.Fatalf("the condition is %+v, want new_tool alone, added, seen at %s", got, seen)
	}
}

// A SERVER THIS NODE CANNOT JUDGE IS MARKED UNREAD, not counted clear: every
// server before the listings are read, and a configured server no live node
// lists (stopped, or not started yet), so a stop or a boot does not clear a
// change that still stands.
func TestAServerThisNodeCannotJudgeIsUnread(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	company := specCompany(t, `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp}
  - {name: docs, command: docs-mcp}
`)
	e, _ := reviewingEngine(t, company)
	conds := []statelog.MCPServerCondition{{Server: "tracker"}, {Server: "docs"}}
	e.changedConditions(ctx, company, conds)
	if !conds[0].ChangesUnread || !conds[1].ChangesUnread {
		t.Fatalf("before the listings are read the conditions are %+v, want both unread", conds)
	}

	e.baselineMCPTools(ctx, map[string]nodeListing{
		"tracker": fakeListing("tracker", "node-a", 1, "close_items")}, time.Now())
	waitFor(t, "the first listing to be accepted", func() bool {
		index, _ := e.reviewIndex()
		return index.Baselined("tracker")
	})
	e.listings.mu.Lock()
	e.listings.answered, e.listings.presenceRead = true, true
	e.listings.presence = []coord.Lease{{Resource: coord.NodeResource("node-a"), Epoch: 1}}
	e.listings.fleet = map[string]coord.MCPListing{
		coord.MCPListingKey("tracker", "node-a"): fakeListing("tracker", "node-a", 1, "close_items").listing}
	e.listings.mu.Unlock()
	conds = []statelog.MCPServerCondition{{Server: "tracker"}, {Server: "docs"}}
	e.changedConditions(ctx, company, conds)
	if conds[0].ChangesUnread || conds[0].Changes != 0 {
		t.Errorf("a listed server as accepted is %+v, want judged with no change", conds[0])
	}
	if !conds[1].ChangesUnread {
		t.Errorf("a server no live node lists is %+v, want unread", conds[1])
	}
}
