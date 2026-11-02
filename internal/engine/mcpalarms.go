package engine

import (
	"context"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE MCP ALARMS' INPUTS (M11): each configured server's condition joined
// across every live node's heartbeat row for it, which the alarm table judges
// at the thresholds the restart schedule and the provisioning pass already keep
// ([statelog.KindMCPServerFailing] and the three beside it).
//
// FLEET-WIDE AND NOTHING ELSE: an instance is down on the node running it and a
// wait is held on the node holding its seat, and every node publishes its
// servers on every beat, so one listing of the presence leases is every
// server's condition, and every data node judges the same one. Read off the same
// listing as the model alarms ([Engine.entriesReading]), which carries the last
// good one across a blip.
//
// THE SUMS ARE THE ANSWER'S: `mcp_servers_status` decides a server's state from
// the same three counts over the same rows, so the alarm and the page cannot
// call one server two things.

// mcpConditions joins every node's MCP rows into one condition per server the
// active configuration runs, in name order. A server turned off raises nothing,
// so it has no condition; one only a node on another revision reports is that
// node's apply, which the control plane names, and has none either.
//
// grants are the servers' OAuth grants by server ([Engine.grantsReading]): a
// server signed in to with OAuth gets its sign-in's condition from its own,
// or none held.
func mcpConditions(c *Company, statuses map[string]coord.NodeStatus,
	grants map[string]coord.MCPGrant) []statelog.MCPServerCondition {

	servers := make([]string, 0, len(c.Config.MCPServers))
	signedIn := map[string]bool{}
	for i := range c.Config.MCPServers {
		if c.Config.MCPServers[i].IsEnabled() {
			servers = append(servers, c.Config.MCPServers[i].Name)
			signedIn[c.Config.MCPServers[i].Name] = c.Config.MCPServers[i].OAuth != nil
		}
	}
	slices.Sort(servers)
	nodes := slices.Sorted(maps.Keys(statuses))
	out := make([]statelog.MCPServerCondition, 0, len(servers))
	for _, server := range servers {
		cond := statelog.MCPServerCondition{Server: server}
		refused := map[string]struct{}{}
		for _, node := range nodes {
			for _, row := range statuses[node].MCP {
				if row.Server != server {
					continue
				}
				cond.Started += row.Started
				cond.Failed += row.Failed
				cond.Waiting += row.Waiting
				// THE INSTANCE DOWN LONGEST ACROSS THE FLEET, ties to the
				// lower node id: each row already names its own node's
				// longest, with its failure, seat and class beside it.
				if row.Failed > 0 && datedBefore(row.DownSince, cond.DownSince) {
					cond.DownSince = row.DownSince
					cond.Down = statelog.MCPInstanceNote{Node: node, Seat: c.Org.AgentHandleByID(row.ErrorSeat),
						Class: row.ErrorClass, Said: row.Error}
				}
				if row.Waiting > 0 && datedBefore(row.WaitingSince, cond.WaitingSince) {
					cond.WaitingSince = row.WaitingSince
					cond.Wait = statelog.MCPInstanceNote{Node: node, Seat: c.Org.AgentHandleByID(row.WaitSeat),
						Said: row.Wait}
				}
				for _, r := range row.Refused {
					refused[r.Tool] = struct{}{}
				}
				cond.RefusedCount = max(cond.RefusedCount, row.RefusedCount)
			}
		}
		cond.Refused = slices.Sorted(maps.Keys(refused))
		cond.RefusedCount = max(cond.RefusedCount, len(cond.Refused))
		if signedIn[server] {
			cond.SignIn = signInCondition(grants, server)
		}
		out = append(out, cond)
	}
	return out
}

// signInCondition is a server's sign-in as its grant record holds it, or
// none held.
func signInCondition(grants map[string]coord.MCPGrant, server string) *statelog.MCPSignInCondition {
	g, held := grants[server]
	if !held {
		return &statelog.MCPSignInCondition{}
	}
	out := &statelog.MCPSignInCondition{Held: true, Failure: g.Failure}
	if g.FailedAt != nil {
		out.FailedAt = *g.FailedAt
	}
	if g.Expiry != nil {
		out.Expiry = *g.Expiry
	}
	return out
}

// grantsReading is every MCP server's OAuth grant, by server, for an alarm
// reading: read afresh where c signs in to any server, and the last good
// reading carried across a store that cannot answer, as the heartbeats are.
// Nil where no server is signed in to with OAuth.
func (e *Engine) grantsReading(ctx context.Context, c *Company) map[string]coord.MCPGrant {
	if !slices.ContainsFunc(c.Config.MCPServers, func(s config.MCPServer) bool { return s.OAuth != nil }) ||
		e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	in := &e.entryInputs
	grants, err := e.backends.Fleet.ListMCPGrants(ctx)
	in.mu.Lock()
	defer in.mu.Unlock()
	if err != nil {
		log.WarnContext(ctx, "mcp_alarms_grants_unread", "error", err,
			"detail", "the MCP sign-in alarms are judged on the last grants this node read")
		return in.grants
	}
	in.grants = make(map[string]coord.MCPGrant, len(grants))
	for _, g := range grants {
		in.grants[g.Server] = g
	}
	return in.grants
}
