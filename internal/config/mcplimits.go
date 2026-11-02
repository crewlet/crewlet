package config

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
)

// A TEAM'S LEAD NARROWS A SERVER; ONLY ITS ENTRY GRANTS IT.
//
// `mcp_limits` on a seat or a unit holds the MCP servers its agent seats are
// granted to less than their entries give: a server turned off
// (`enabled: false`), a tool at `approve` or `blocked` ([org.MCPLimits]). Each
// agent seat is held to the strictest of its own block, every enclosing unit's
// at any depth, and the server's own entry: [MCPServer.Grants] reads a limit
// that turns its server off, and the engine holds each tool to the
// stricter of its entry's level and the seat's limit where it cuts the seat's
// surface (internal/engine, `grantedSurface`).
//
// A LIMIT IS NOT A GRANT, so it is held to no grant: a team's lead may limit a
// server before an administrator grants it to the team, and the limit holds
// from the day the grant does. What a limit names must be a server the company
// declares ([Company.validateMCPServerReferences]), so a removal of the server
// clears its limits with it.
//
// A LEAD MAY ONLY NARROW. Seats and units are written under the org chart's
// own verb, so a team's lead writes their own team's limits; a write that
// lifts or loosens what any agent seat is held to is listed apart by
// [DiffOrg] ([OrgDiff.Widened]) and is an administrator's.

// validateMCPLimits holds every `mcp_limits` block to what a limit can say,
// and refuses an executor box listing of a server some of whose tools a seat's
// limits hold back.
//
//   - A block holding nothing back is refused: it reads as a limit and is
//     none. So is `enabled: true`, which a limit cannot say: only the
//     server's own entry turns it on.
//   - A tool is held at `approve` or `blocked`, and keyed by a name a server
//     can list. `allow` is refused: it is the level the server's own entry
//     sets, and a limit never loosens.
//   - A box reaches a server whole, past the engine, so a server a seat's
//     limits hold a tool of back is not given to its box, as a server whose
//     entry holds one back is not ([Company.validateBoxToolLevels]).
//
// An ADMISSION rule: a stored revision that breaks it runs and warns. Its
// limits hold as written, a level this build cannot read holding the tool
// back as far as any does ([org.ToolLimit]), and its box is given no such
// server.
func (c *Company) validateMCPLimits(o *org.Organization) error {
	var p problems
	for role, path := range c.EachRole() {
		validateLimits(&p, at(path, "mcp_limits"), role.MCPLimits)
	}
	for unit, path := range c.EachUnit() {
		validateLimits(&p, at(path, "mcp_limits"), unit.MCPLimits)
	}
	servers := map[string]*MCPServer{}
	for i := range c.MCPServers {
		servers[c.MCPServers[i].Name] = &c.MCPServers[i]
	}
	for role, path := range c.EachRole() {
		if role.Sandbox == nil {
			continue
		}
		seat := o.Role(role.IdentityKey())
		for j, name := range role.Sandbox.MCP.Servers {
			server, declared := servers[name]
			// A SERVER ITS ENTRY ALREADY HOLDS BACK is refused in the box
			// by that rule, in its words; one refusal at a place is enough.
			if !declared || !server.Tools.AllowsEveryTool() {
				continue
			}
			limit := seat.LimitOn(name)
			if len(limit.Tools) == 0 {
				continue
			}
			p.add(idx(at(path, "sandbox.mcp.servers"), j), ErrConflict,
				"the limits on this seat hold tools of %q back (%s), and an executor box reaches a "+
					"server whole: the harness in the box dials it itself, past the engine, so it "+
					"would reach the tools held back and call a tool that asks a person first without "+
					"asking. Take %q out of the box, where the seat still reaches its tools over the "+
					"engine's bridge, each at its level", name, heldBy(limit), name)
		}
	}
	return p.err()
}

// validateLimits holds one seat's or unit's `mcp_limits`, written at path.
func validateLimits(p *problems, path Path, limits org.MCPLimits) {
	for _, server := range slices.Sorted(maps.Keys(limits)) {
		here := entry(path, server)
		limit := limits[server]
		if limit.Enabled.Or(false) {
			p.add(at(here, "enabled"), ErrConflict, "a limit cannot turn %q on: only its own "+
				"entry does. Write enabled: false to turn it off for these seats, or leave "+
				"enabled out", server)
		} else if limit.IsZero() {
			p.add(here, ErrMissing, "holds nothing back: set enabled: false to turn %q off, "+
				"hold one of its tools at approve or blocked, or remove the entry", server)
			continue
		}
		for _, tool := range slices.Sorted(maps.Keys(limit.Tools)) {
			key := entry(at(here, "tools"), tool)
			if level := limit.Tools[tool]; !level.Valid() {
				p.add(key, ErrUnknownValue, "%q (want %s): a limit holds a tool back, and "+
					"allow is the level the server's own entry sets", level, words.Or(org.ToolLimits))
			}
			toolKey(p, key, tool)
		}
	}
}

// toolKey refuses a key that names no tool a server can list: nothing, or a
// name holding a space or a control character, which no tool name carries. A
// well-formed name the server does not list is a fact about the server, not
// the document, and is reported beside the listing instead.
func toolKey(p *problems, path Path, tool string) {
	if strings.TrimSpace(tool) == "" {
		p.add(path, ErrMissing, "an empty key names no tool: key a level by the "+
			"server's own name for the tool")
	} else if strings.ContainsFunc(tool, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		p.add(path, ErrUnknownValue, "%q holds a space or a control character, which "+
			"no tool name does: key a level by the server's own name for the tool", tool)
	}
}

// heldBy is each tool a seat's limits hold back, sorted, with its level and
// the block that holds it there, in a person's words.
func heldBy(limit org.ServerLimit) string {
	named := make([]words.Said, 0, len(limit.Tools))
	for _, tool := range slices.Sorted(maps.Keys(limit.Tools)) {
		held := limit.Tools[tool]
		by := "the seat's own limits"
		if held.By != "" {
			by = fmt.Sprintf("unit %q's limits", held.By)
		}
		named = append(named, words.Said(fmt.Sprintf("%q at %s by %s", tool, held.Level, by)))
	}
	return words.And(named)
}

// widened is every limit turning before into after lifts or loosens for an
// agent seat both hold, judged by the servers after declares
// ([OrgDiff.Widened]).
//
// ONLY WHERE IT CHANGES WHAT THE SEAT GETS:
//   - a server that reaches the seat after, whether or not it is on
//     ([MCPServer.Reach]), since a limit lifted from a server nobody grants
//     the seat hands it nothing, while one lifted from a server turned off
//     hands it the server the day it is turned back on;
//   - a tool whose entry does not already hold it as far, by its own level or
//     the entry's default: what the entry leaves to a tool's hints is decided
//     from a listing no document holds, so it is taken to run at allow, and a
//     limit lifted there is a widening;
//   - and no tool of a server still turned off for the seat, nor of one it was
//     turned off before, which is listed once, as the server.
func widened(before, after *org.Organization, c *Company) []OrgWidening {
	if before == nil || after == nil {
		return nil
	}
	servers := map[string]*MCPServer{}
	for i := range c.MCPServers {
		servers[c.MCPServers[i].Name] = &c.MCPServers[i]
	}
	var out []OrgWidening
	for seat := range after.AllRoles() {
		was := before.Role(seat.Handle())
		if !seat.IsAgent() || was == nil || !was.IsAgent() {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(was.Limits)) {
			server, declared := servers[name]
			if !declared {
				continue
			}
			if _, reached := server.Reach(seat); !reached {
				continue
			}
			held, now := was.Limits[name], seat.LimitOn(name)
			switch {
			case now.Off:
				continue
			case held.Off:
				out = append(out, OrgWidening{Seat: seat.Handle(), Server: name})
				continue
			}
			for _, tool := range slices.Sorted(maps.Keys(held.Tools)) {
				level := held.Tools[tool].Level
				if still, ok := now.Tool(tool); ok && !level.Stricter(still) {
					continue
				}
				if server.Tools.holds(tool, level) {
					continue
				}
				out = append(out, OrgWidening{Seat: seat.Handle(), Server: name, Tool: tool})
			}
		}
	}
	slices.SortFunc(out, func(a, b OrgWidening) int {
		return cmp.Or(cmp.Compare(a.Seat, b.Seat), cmp.Compare(a.Server, b.Server), cmp.Compare(a.Tool, b.Tool))
	})
	return out
}
