package queries_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/toolname"
)

// agentsCompany grants four servers four ways: docs to every agent with one
// tool blocked, tracker to the eng unit, wiki (turned off) to the sales lead,
// and github, one instance per agent, to dev and the lead by handle.
const agentsCompany = `
name: Acme
mcp_servers:
  - {name: docs, command: docs-mcp, tools: {levels: {delete_page: blocked}}}
  - {name: tracker, command: tracker-mcp, access: {units: [eng]}}
  - {name: wiki, command: wiki-mcp, enabled: false, access: {seats: [lead]}}
  - {name: github, command: gh-mcp, shared: false, tool_prefix: gh_, access: {seats: [dev, lead]}}
roles:
  - {name: Founder, handle: founder, kind: human}
  - {name: Chief of Staff, handle: chief}
units:
  - name: Engineering
    id: eng
    roles:
      - {name: Dev, handle: dev}
  - name: Sales
    id: sales
    roles:
      - {name: Lead, handle: lead}
`

// agentsSources answers over doc on a fleet of two reporting nodes, whose live
// listings are the ones listings returns, given each node's presence epoch.
func agentsSources(t *testing.T, doc string, statuses map[string][]coord.MCPServerStatus,
	listings func(live map[string]int64) []coord.MCPListing,
) queries.Sources {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	backend := coordmemory.New()
	for _, node := range []string{"node-a", "node-b"} {
		presentNode(t, backend, node, &coord.NodeStatus{MCP: statuses[node]})
	}
	return queries.Sources{
		Coord: backend, Company: companySource(t, cfg),
		Builtins:    catalogueOffer,
		MCPListings: fakeListings{listings: listings(epochs(t, backend))},
	}
}

// tool is one tool a listing names, blocked or filed.
func tool(name, raw string, blocked bool) coord.MCPListedTool {
	return coord.MCPListedTool{Name: name, Raw: raw, Hints: unannotated, Digest: digestFor(name),
		Instances: 1, Blocked: blocked}
}

func agentNamed(t *testing.T, a queries.MCPStatusAnswer, seat string) queries.MCPAgentTools {
	t.Helper()
	i := slices.IndexFunc(a.Agents, func(r queries.MCPAgentTools) bool { return r.Seat == seat })
	if i < 0 {
		t.Fatalf("no agent %s in %+v", seat, a.Agents)
	}
	return a.Agents[i]
}

// AN AGENT'S COUNT LEAVES OUT BLOCKED AND REFUSED TOOLS, AND A SERVER'S TOOLS
// REACH IT ONLY THROUGH ITS GRANT.
//
// docs lists three tools on each node, the entry blocking one, and refuses a
// fourth its name, which is never listed, so every agent it reaches counts
// two; a listing a node that is gone left behind counts nothing. tracker
// reaches the eng unit alone; wiki is turned off, so the lead it names is
// shown the grant and counts nothing; github's children list what each seat's
// credential reaches, and each agent it reaches counts the whole listing. A
// name two servers offer is one tool, so dev's count is not the sum of its
// servers'. A human seat runs no tools and is not an agent.
//
// Mutation: count a blocked tool, count a server through Reach rather than
// Grants, count a listing whatever its node's presence, or sum the servers'
// counts, and this is red.
func TestAnAgentsCountLeavesOutBlockedAndRefusedToolsAndOnlyItsGrantsCount(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	docs := []coord.MCPListedTool{tool("delete_page", "delete_page", true), tool("read_page", "read_page", false),
		tool("search", "search", false)}
	s := agentsSources(t, agentsCompany, map[string][]coord.MCPServerStatus{
		"node-a": {{Server: "docs", Shared: true, Started: 1, Tools: 2,
			Refused: []coord.MCPToolRefusal{{Tool: "read.page", Reason: "characters"}}, RefusedCount: 1}},
	}, func(live map[string]int64) []coord.MCPListing {
		return []coord.MCPListing{
			{Server: "docs", Node: "node-a", Epoch: live["node-a"], At: at, Shared: true, Instances: 1,
				Tools: docs},
			{Server: "docs", Node: "node-b", Epoch: live["node-b"], At: at, Shared: true, Instances: 1,
				Tools: docs},
			// A NODE THAT IS GONE: its record is still there, and counts
			// for nobody.
			{Server: "docs", Node: "node-gone", Epoch: 7, At: at, Shared: true, Instances: 1,
				Tools: []coord.MCPListedTool{tool("drop_all", "drop_all", false)}},
			{Server: "tracker", Node: "node-a", Epoch: live["node-a"], At: at, Shared: true, Instances: 1,
				Tools: []coord.MCPListedTool{tool("file_issue", "file_issue", false),
					tool("search", "search", false)}},
			// A NODE STILL ON THE REVISION BEFORE wiki WAS TURNED OFF.
			{Server: "wiki", Node: "node-b", Epoch: live["node-b"], At: at, Shared: true, Instances: 1,
				Tools: []coord.MCPListedTool{tool("edit", "edit", false)}},
			{Server: "github", Node: "node-a", Epoch: live["node-a"], At: at, Instances: 1,
				Tools: []coord.MCPListedTool{tool("gh_create_issue", "create_issue", false),
					tool("gh_get_issue", "get_issue", false)}},
			{Server: "github", Node: "node-b", Epoch: live["node-b"], At: at, Instances: 1,
				Tools: []coord.MCPListedTool{tool("gh_get_issue", "get_issue", false),
					tool("gh_merge", "merge", false)}},
		}
	})
	got := askMCPStatus(t, s)
	builtins := 0
	for _, g := range got.Builtin {
		builtins += len(g.Tools)
	}
	if builtins == 0 {
		t.Fatal("the fixture's node offers no built-in, so builtin_tools proves nothing")
	}

	str := func(v string) *string { return &v }
	state := func(server string) queries.MCPServerState { return serverRow(t, got, server).State }
	want := []queries.MCPAgentTools{
		{Seat: "chief", Name: "Chief of Staff", Servers: []queries.MCPAgentServer{
			{Server: "docs", Reason: config.MCPReachEveryAgent, Tools: 2, State: state("docs")},
		}, MCPTools: 2},
		{Seat: "dev", Name: "Dev", Unit: str("Engineering"), UnitKey: str("eng"), Servers: []queries.MCPAgentServer{
			{Server: "docs", Reason: config.MCPReachEveryAgent, Tools: 2, State: state("docs")},
			{Server: "github", Reason: config.MCPReachSeat, Tools: 3, State: state("github")},
			{Server: "tracker", Reason: config.MCPReachUnit, Tools: 2, State: state("tracker")},
		}, MCPTools: 6},
		{Seat: "lead", Name: "Lead", Unit: str("Sales"), UnitKey: str("sales"), Servers: []queries.MCPAgentServer{
			{Server: "docs", Reason: config.MCPReachEveryAgent, Tools: 2, State: state("docs")},
			{Server: "github", Reason: config.MCPReachSeat, Tools: 3, State: state("github")},
			{Server: "wiki", Reason: config.MCPReachSeat, Tools: 0, State: queries.MCPDisabled},
		}, MCPTools: 5},
	}
	for i := range want {
		want[i].BuiltinTools = builtins
	}
	if raw, wantRaw := mustJSON(t, got.Agents), mustJSON(t, want); string(raw) != string(wantRaw) {
		t.Errorf("agents =\n%s\nwant\n%s", raw, wantRaw)
	}
	if got.ToolBudget != queries.MCPToolBudget || queries.MCPToolBudget != 100 {
		t.Errorf("tool_budget = %d (the constant is %d), want 100", got.ToolBudget, queries.MCPToolBudget)
	}
}

// EACH AGENT IS COUNTED THE BUILT-INS IT IS OFFERED: every tool on the
// answer's shelves but those the offer withholds from it, decided over the
// chart the answer lists it in. dev is withheld one shelved tool; lead is
// withheld one shelved tool, a row the catalogue shelves nowhere (submit_work)
// and a built-in no row carries (fleet_health), the last two taking nothing
// off; chief is withheld nothing. The withholding is asked once for each agent
// and never for a person.
//
// Mutation: stamp one total on every agent (dev counted 4), count the shelved
// less the withheld (lead counted 1), count rows rather than shelves (chief
// counted 5), or ask over another chart or for a human seat, and this is red.
func TestEachAgentIsCountedTheBuiltinsItIsOffered(t *testing.T) {
	t.Parallel()
	s := agentsSources(t, agentsCompany, nil, func(map[string]int64) []coord.MCPListing { return nil })
	var mu sync.Mutex
	var asked []string
	s.Builtins = func() queries.BuiltinOffer {
		return queries.BuiltinOffer{Rows: catalogueBuiltins(),
			Withheld: func(_ context.Context, o *org.Organization, seat *org.Role) []string {
				mu.Lock()
				defer mu.Unlock()
				asked = append(asked, seat.Handle())
				if o == nil || o.AgentSeatByHandle(seat.Handle()) != seat {
					t.Errorf("%s was decided over a chart that does not list it", seat.Handle())
				}
				switch seat.Handle() {
				case "dev":
					return []string{builtin.CreateWorkItemTool}
				case "lead":
					return []string{builtin.ShareArtifactTool, toolname.SubmitWork, builtin.FleetHealthTool}
				}
				return nil
			}}
	}
	got := askMCPStatus(t, s)
	shelved := 0
	for _, g := range got.Builtin {
		shelved += len(g.Tools)
	}
	if shelved != 4 {
		t.Fatalf("the premise: the answer shelves %d built-ins, want 4 (list and create, share, activate)",
			shelved)
	}
	for seat, want := range map[string]int{"chief": 4, "dev": 3, "lead": 3} {
		if n := agentNamed(t, got, seat).BuiltinTools; n != want {
			t.Errorf("%s is counted %d built-ins, want %d", seat, n, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	slices.Sort(asked)
	if !slices.Equal(asked, []string{"chief", "dev", "lead"}) {
		t.Errorf("the withholding was asked for %v, want chief, dev and lead once each, and never a person",
			asked)
	}
}

// listingOf is one node's listing of server naming n tools t000, t001, ....
func listingOf(server string, epoch int64, n int) coord.MCPListing {
	tools := make([]coord.MCPListedTool, 0, n)
	for i := range n {
		name := fmt.Sprintf("t%03d", i)
		tools = append(tools, tool(name, name, false))
	}
	return coord.MCPListing{Server: server, Node: "node-a", Epoch: epoch, Shared: true, Instances: 1,
		At: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC), Tools: tools}
}

// AN AGENT PAST THE BUDGET IS FLAGGED, AND ONE AT IT IS NOT: a hundred tools is
// the budget, and the hundred-and-first is past it. Blocking one tool brings
// the agent back to it.
//
// Mutation: flag an agent at the budget (>=), or count a blocked tool, and
// this is red.
func TestAnAgentPastTheBudgetIsFlaggedAndOneAtItIsNot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		levels string
		tools  int
		over   bool
	}{
		{"a hundred and one tools", "", 101, true},
		{"one of them blocked", ", tools: {levels: {t000: blocked}}", 100, false},
		{"a hundred tools", "", 100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			listed := 101
			if tc.levels == "" {
				listed = tc.tools
			}
			doc := "name: Acme\nmcp_servers:\n  - {name: big, command: big-mcp" + tc.levels + "}\n" +
				"roles:\n  - {name: Ops, handle: ops}\n"
			s := agentsSources(t, doc, nil, func(live map[string]int64) []coord.MCPListing {
				return []coord.MCPListing{listingOf("big", live["node-a"], listed)}
			})
			ops := agentNamed(t, askMCPStatus(t, s), "ops")
			if ops.MCPTools != tc.tools || ops.OverBudget != tc.over {
				t.Errorf("ops holds %d tools, over budget %v; want %d, %v", ops.MCPTools, ops.OverBudget,
					tc.tools, tc.over)
			}
		})
	}
}

// A SERVER LISTING HUNDREDS OF TOOLS COUNTS EVERY ONE BY NAME: a listing names
// every tool its node's instances list, so an agent it reaches is counted all
// 300, and a server that blocks every tool but two counts those alone.
//
// Mutation: count only the first 256 a listing once named, and this is red.
func TestAServerListingHundredsOfToolsCountsEveryOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		levels string
		want   int
	}{
		{"every tool offered", "", 300},
		{"every tool blocked but two", ", tools: {default: blocked, levels: {t000: allow, t001: allow}}", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc := "name: Acme\nmcp_servers:\n  - {name: big, command: big-mcp" + tc.levels + "}\n" +
				"roles:\n  - {name: Ops, handle: ops}\n"
			s := agentsSources(t, doc, nil, func(live map[string]int64) []coord.MCPListing {
				l := listingOf("big", live["node-a"], 300)
				for i := 2; i < len(l.Tools); i++ {
					l.Tools[i].Blocked = tc.levels != ""
				}
				return []coord.MCPListing{l}
			})
			ops := agentNamed(t, askMCPStatus(t, s), "ops")
			if ops.MCPTools != tc.want || ops.Servers[0].Tools != tc.want {
				t.Errorf("ops holds %d tools, %d from big; want %d", ops.MCPTools, ops.Servers[0].Tools, tc.want)
			}
		})
	}
}

// THE AGENTS ARE UNKNOWN, NOT TOOL-LESS, WHERE THE LISTINGS CANNOT BE READ: a
// node whose copy of the fleet's listings has not been read yet answers null,
// never every agent at zero. A company declaring no server needs no listing
// to say every agent holds none.
//
// Mutation: answer the agents at zero over an unread copy, and this is red.
func TestTheAgentsAreUnknownWhereTheListingsCannotBeRead(t *testing.T) {
	t.Parallel()
	s := agentsSources(t, agentsCompany, nil, func(map[string]int64) []coord.MCPListing { return nil })
	s.MCPListings = fakeListings{err: errors.New("not yet read")}
	got := asMap(t, askMCPStatus(t, s))
	if agents, ok := got["agents"]; !ok || agents != nil {
		t.Errorf("agents over an unread copy = %v, want null", agents)
	}
	if got["tool_budget"] != float64(queries.MCPToolBudget) {
		t.Errorf("tool_budget = %v, want the budget whatever the agents", got["tool_budget"])
	}

	none := agentsSources(t, "name: Acme\nroles:\n  - {name: Ops, handle: ops}\n", nil,
		func(map[string]int64) []coord.MCPListing { return nil })
	none.MCPListings = nil
	agents := askMCPStatus(t, none).Agents
	if raw := mustJSON(t, agents); len(agents) != 1 || agents[0].MCPTools != 0 || agents[0].Servers == nil {
		t.Errorf("a company with no server answers agents %s, want ops holding none", raw)
	}
}

// AN AGENT'S COUNT FOLLOWS ITS LIMITS, as its surface is cut by them: a tool
// its own or its unit's limits block, keyed by the server's own name before
// tool_prefix, is not counted, one held at approve still is (it is offered and
// each call waits), and a server a limit turns off counts nothing while its
// grant is still shown. An agent the limits do not hold counts every tool.
//
// Mutation: count the offer whatever the seat's limits, or match a limit
// against the prefixed name, and this is red.
func TestAnAgentsCountFollowsItsLimits(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
mcp_servers:
  - {name: github, command: gh-mcp, tool_prefix: gh_, tools: {default: allow}}
  - {name: docs, command: docs-mcp, tools: {default: allow}}
roles:
  - {name: Chief of Staff, handle: chief}
units:
  - name: Engineering
    id: eng
    mcp_limits:
      github: {tools: {merge: blocked, create_issue: approve}}
    roles:
      - name: Dev
        handle: dev
        mcp_limits:
          docs: {enabled: false}
`
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	s := agentsSources(t, doc, nil, func(live map[string]int64) []coord.MCPListing {
		return []coord.MCPListing{
			{Server: "github", Node: "node-a", Epoch: live["node-a"], At: at, Shared: true, Instances: 1,
				Tools: []coord.MCPListedTool{tool("gh_create_issue", "create_issue", false),
					tool("gh_get_issue", "get_issue", false), tool("gh_merge", "merge", false)}},
			{Server: "docs", Node: "node-a", Epoch: live["node-a"], At: at, Shared: true, Instances: 1,
				Tools: []coord.MCPListedTool{tool("read_page", "read_page", false)}},
		}
	})
	got := askMCPStatus(t, s)
	for seat, want := range map[string]map[string]int{
		"chief": {"github": 3, "docs": 1, "": 4},
		"dev":   {"github": 2, "docs": 0, "": 2},
	} {
		agent := agentNamed(t, got, seat)
		for _, server := range agent.Servers {
			if server.Tools != want[server.Server] {
				t.Errorf("%s counts %d of %s's tools, want %d", seat, server.Tools, server.Server, want[server.Server])
			}
		}
		if len(agent.Servers) != 2 {
			t.Errorf("%s is shown %d servers, want both grants whatever the limits", seat, len(agent.Servers))
		}
		if agent.MCPTools != want[""] {
			t.Errorf("%s holds %d MCP tools, want %d", seat, agent.MCPTools, want[""])
		}
	}
}
