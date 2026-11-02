package engine

import (
	"cmp"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/sandbox"
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
func mcpConditions(c *Company, statuses map[string]coord.NodeStatus) []statelog.MCPServerCondition {
	servers := make([]string, 0, len(c.Config.MCPServers))
	for i := range c.Config.MCPServers {
		if c.Config.MCPServers[i].IsEnabled() {
			servers = append(servers, c.Config.MCPServers[i].Name)
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
		out = append(out, cond)
	}
	return out
}

// heldConditions lays the calls the fleet's runs hold for a person's approval
// over each server's condition (ADR-0077): how many still wait on an answer,
// and the one waiting longest, ties to the lower turn id so every data node
// names the same one. A call already answered, or whose run is not parked on
// it, waits on nobody; a server no condition names (turned off, or not this
// revision's) raises nothing.
//
// OFF THE RUNS, NOT THE HEARTBEAT: a held call is a run's question, on the
// run's row in the coordination store, which every node reads alike.
func heldConditions(conds []statelog.MCPServerCondition, runs []sandbox.PendingRun) {
	at := make(map[string]int, len(conds))
	for i := range conds {
		at[conds[i].Server] = i
	}
	runs = slices.Clone(runs)
	slices.SortFunc(runs, func(a, b sandbox.PendingRun) int { return cmp.Compare(a.TurnID, b.TurnID) })
	for _, run := range runs {
		held := run.HeldCall
		if held == nil || held.LaunchID != run.LaunchID || run.Answer != nil ||
			!slices.Contains(sandbox.Awaiting, run.Status) {
			continue
		}
		i, ok := at[held.Server]
		if !ok {
			continue
		}
		cond := &conds[i]
		cond.Approvals++
		if datedBefore(held.HeldAt, cond.ApprovalSince) {
			cond.ApprovalSince = held.HeldAt
			cond.Approval = statelog.MCPApprovalNote{Seat: run.AgentHandle, Tool: held.Tool}
		}
	}
}
