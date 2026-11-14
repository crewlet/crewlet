// What each agent seat is handed by the MCP servers its grants reach, counted
// against the tool budget, and how many of the engine's own tools it is
// offered beside them.

package queries

import (
	"cmp"
	"context"
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
)

// MCPToolBudget is how many MCP tools one agent may be handed before
// `mcp_servers_status` flags it ([MCPAgentTools.OverBudget]).
//
// ONE HUNDRED, the lowest cap a widely used agent client puts on the tools it
// hands a model: Windsurf loads at most 100 across all of a person's MCP
// servers and leaves the rest unloaded, under the 128 VS Code is widely
// reported to stop at and the 128 OpenAI's API takes in one request. A seat's
// harness defers MCP tools behind its own search, so nothing breaks past it;
// what degrades is the model's choice among a long list, which is why it is a
// warning on the answer and never a refusal anywhere.
//
// A CONSTANT CARRIED ON THE ANSWER, never configured: no company has a reason
// to judge a long list differently from another, and a screen reads it from
// the answer ([MCPStatusAnswer.ToolBudget]) rather than writing its own copy.
const MCPToolBudget = 100

// MCPAgentTools is one agent seat's MCP tools: every server whose grant
// reaches it, what each hands it, and the count the budget is judged on, and
// how many of the engine's own tools it is offered.
type MCPAgentTools struct {
	// Seat is the seat's handle and Name its display name.
	Seat string `json:"seat"`
	Name string `json:"name"`
	// Unit is the name of the unit the seat sits in and UnitKey that unit's
	// key, the one a server's `access.units` names it by: both null for a
	// seat at the root of the chart.
	Unit    *string `json:"unit"`
	UnitKey *string `json:"unit_key"`
	// Servers is every server the configuration declares whose grant reaches
	// the seat ([config.MCPServer.Reach]), on or off, in name order. ALWAYS A
	// LIST: a seat no server reaches has none.
	Servers []MCPAgentServer `json:"servers"`
	// MCPTools is how many MCP tools the seat is handed: every tool each
	// server it is granted offers it, a name two servers offer counted once,
	// since the seat is handed one tool under it. BuiltinTools is how many of
	// the engine's own tools on the answer's shelves ([MCPStatusAnswer.Builtin])
	// the seat is offered: every one but those its turns' surfaces leave off
	// ([BuiltinOffer.Withheld]), so it differs from seat to seat. The budget
	// counts none of them: they are the engine's, not a server's.
	MCPTools     int `json:"mcp_tools"`
	BuiltinTools int `json:"builtin_tools"`
	// OverBudget is whether MCPTools is past [MCPToolBudget].
	OverBudget bool `json:"over_budget"`
}

// MCPAgentServer is one server's part in an agent's tools.
type MCPAgentServer struct {
	Server string                `json:"server"`
	Reason config.MCPReachReason `json:"reason"`
	// Tools is how many tools the server offers the seat: every tool the
	// fleet's live listings name for it at a level other than blocked, one
	// at approve included (it is offered, and each call waits for a person),
	// but one the seat's own limits block for it ([org.Role.LimitOn]); a
	// tool refused its name is never listed. 0 for a server turned off,
	// which is granted to nobody, and for one a limit turns off for the seat.
	Tools int `json:"tools"`
	// State is the server's own state, its row's ([MCPServerRow.State]): an
	// agent's own instance of a per-agent server is the server's page's to
	// show, read where it runs.
	State MCPServerState `json:"state"`
}

// mcpAgents is every agent seat's MCP tools, in the chart's order: the ONE
// reading of what each agent's grants hand it, which `mcp_servers_status`
// carries so no screen counts them again.
//
// A SERVER'S TOOLS REACH A SEAT ONLY THROUGH ITS GRANT ([config.MCPServer.Grants]),
// the rule the engine builds the seat's surface and starts its instances by,
// and are counted as the fleet's live listings name them at the level the
// server's entry decides ([serverTools]), the reading a server's page lists
// them by, less each tool the seat's own limits block for it: the seat's
// surface is cut by the same limits ([org.Role.LimitOn]).
//
// WHAT THE GRANT HANDS THE SEAT, WHATEVER ITS INSTANCES ARE DOING NOW: a server
// that is down still lists what it served, and its tools come back with it, so
// an outage moves no agent across the budget. A PER-AGENT SERVER IS COUNTED AT
// ITS WHOLE LISTING for every agent it reaches: its children each list what
// their own seat's credential reaches, which no record the fleet shares says
// seat by seat (the heartbeat carries nothing a seat count multiplies, and an
// instance is read only where it runs), and the whole listing is the most an
// agent of it is offered, so the budget errs toward flagging, never toward
// missing an agent past it.
//
// EACH AGENT'S BUILT-INS ARE THE ONES IT IS OFFERED, counted on the answer's
// own shelves less what builtins withholds from it, decided over the chart
// this reading lists it in: the engine's one answer, which its turns' surfaces
// are cut by, so the page never counts an agent a tool its turns are not
// offered. A withheld name no shelf carries takes nothing off
// ([builtinsOffered]).
//
// NULL, NOT EMPTY, where this node cannot read the fleet's listings: it keeps
// no copy, or its copy has not been read yet, a moment after it starts. An
// agent counted at nothing there would read as one holding no tools.
func (s Sources) mcpAgents(ctx context.Context, leases []coord.Lease, status MCPStatusAnswer,
	builtins BuiltinOffer,
) []MCPAgentTools {
	out := []MCPAgentTools{}
	if s.Company == nil {
		return out
	}
	company, roster := s.Company()
	if company == nil || roster == nil {
		return out
	}
	servers := make([]*config.MCPServer, 0, len(company.MCPServers))
	for i := range company.MCPServers {
		servers = append(servers, &company.MCPServers[i])
	}
	slices.SortFunc(servers, func(a, b *config.MCPServer) int { return cmp.Compare(a.Name, b.Name) })
	offers := map[string]mcpOffer{}
	if len(servers) > 0 {
		if s.MCPListings == nil {
			return nil
		}
		all, err := s.MCPListings.MCPListings(ctx)
		if err != nil {
			return nil
		}
		listed := map[string][]coord.MCPListing{}
		for _, l := range coord.LiveMCPListings(all, leases) {
			listed[l.Server] = append(listed[l.Server], l)
		}
		for _, server := range servers {
			offers[server.Name] = offerOf(server, listed[server.Name])
		}
	}
	states := make(map[string]MCPServerState, len(status.Servers))
	for _, row := range status.Servers {
		states[row.Name] = row.State
	}
	for seat := range roster.AllRoles() {
		if !seat.IsAgent() {
			continue
		}
		agent := MCPAgentTools{Seat: seat.Handle(), Name: seat.Name, Servers: []MCPAgentServer{},
			BuiltinTools: builtinsOffered(status.Builtin, builtins.withheldFrom(ctx, roster, seat))}
		agent.Unit, agent.UnitKey = unitOf(seat, roster)
		names := map[string]struct{}{}
		for _, server := range servers {
			reach, ok := server.Reach(seat)
			if !ok {
				continue
			}
			view := MCPAgentServer{Server: server.Name, Reason: reach.Reason, State: states[server.Name]}
			if server.Grants(seat) {
				offer := offers[server.Name]
				offered := offer.to(seat.LimitOn(server.Name))
				view.Tools = len(offered)
				for _, name := range offered {
					names[name] = struct{}{}
				}
			}
			agent.Servers = append(agent.Servers, view)
		}
		agent.MCPTools = len(names)
		agent.OverBudget = agent.MCPTools > MCPToolBudget
		out = append(out, agent)
	}
	return out
}

// builtinsOffered is how many of the tools on shelves an agent is offered when
// withheld names the ones it is not: every shelved tool but one withheld names.
// A withheld name no shelf carries — a built-in the catalogue files nowhere,
// or one this node does not register — takes nothing off.
func builtinsOffered(shelves []BuiltinGroup, withheld []string) int {
	n := 0
	for _, g := range shelves {
		for _, row := range g.Tools {
			if !slices.Contains(withheld, row.Name) {
				n++
			}
		}
	}
	return n
}

// unitOf is the name and key of the unit seat sits in, both nil for a seat at
// the root of the chart.
func unitOf(seat *org.Role, roster *org.Organization) (name, key *string) {
	if len(seat.UnitKeys) == 0 {
		return nil, nil
	}
	k := seat.UnitKeys[0]
	key = &k
	if u := roster.Unit(k); u != nil {
		n := u.Name
		name = &n
	}
	return name, key
}

// mcpOffer is what one server offers each agent its grant reaches, before
// the agent's own limits: the tools offered under each name the listings
// carry, each with the server's own name for it, which a limit keys a tool by.
type mcpOffer struct {
	tools []offeredTool
}

// offeredTool is one tool an offer carries: its name, prefix applied, and the
// server's own.
type offeredTool struct{ name, raw string }

// to is the names of the tools the offer hands one agent whose limits on the
// server are limit: every tool but one a limit blocks.
func (o mcpOffer) to(limit org.ServerLimit) []string {
	out := make([]string, 0, len(o.tools))
	for _, t := range o.tools {
		if level, held := limit.Tool(t.raw); held && level == org.ToolBlocked {
			continue
		}
		out = append(out, t.name)
	}
	return out
}

// offerOf is what server offers each agent it is granted to, as its live
// listings name its tools: every one at a level other than blocked.
func offerOf(server *config.MCPServer, listings []coord.MCPListing) mcpOffer {
	tools, _ := serverTools(listings, server)
	var out mcpOffer
	for _, t := range tools {
		if t.Level != config.MCPToolBlocked {
			out.tools = append(out.tools, offeredTool{name: t.Name, raw: t.Raw})
		}
	}
	return out
}
