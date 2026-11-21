package config

import (
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
)

// EACH TOOL OF AN MCP SERVER AT A LEVEL, set by `tools` on its entry
// ([MCPToolPolicy]): `levels` names a tool by the server's own name (before
// `tool_prefix`), `default` covers the rest. `allow` is offered and runs;
// `approve` is offered and each call waits for a person; `blocked` is never
// filed, so no agent is offered, lists or can call it, even from a box.
//
// A TOOL NOTHING SETS STARTS BY ITS HINTS (D1): read-only per its server, it
// runs at `allow`, every other at `approve` (`mcp.StartingLevel`).
//
// ENFORCED WHERE TOOLS ARE FILED (`Spec.Levels`), so every reader of a seat's
// surface sees one set; an `approve` call parks an executor's run on a person
// and is refused to an engine-host phase (internal/tools, ADR-0077).

// MCPToolLevel is the level a tool of an MCP server runs at.
type MCPToolLevel string

const (
	// MCPToolAllow offers the tool to every agent the server reaches, and
	// runs it when called.
	MCPToolAllow MCPToolLevel = "allow"
	// MCPToolApprove offers the tool, and holds each call until a person
	// answers it: once, always (which sets the tool to allow), or deny.
	MCPToolApprove MCPToolLevel = "approve"
	// MCPToolBlocked never files the tool: no agent is offered it, lists it
	// or can call it.
	MCPToolBlocked MCPToolLevel = "blocked"
)

// MCPToolLevels is the closed set, in the order a reader is told them.
var MCPToolLevels = []MCPToolLevel{MCPToolAllow, MCPToolApprove, MCPToolBlocked}

// Valid reports whether l is one of [MCPToolLevels].
func (l MCPToolLevel) Valid() bool { return slices.Contains(MCPToolLevels, l) }

// MCPToolPolicy is the level of each of a server's tools ([MCPServer.Tools]).
//
// AN EXECUTOR BOX REACHES A SERVER WHOLE: the harness dials it past the engine,
// so a box can neither leave out a tool nor ask before a call. A listing in
// `sandbox.mcp.servers` is refused unless every tool is certain to run at
// `allow` (`default: allow`, no other level; [Company.validateBoxToolLevels]).
// The engine gives a box no such server whatever a stored revision says; the
// seat still reaches its tools over the bridge, each at its level.
type MCPToolPolicy struct {
	// Default is the level of every tool Levels names none for. Unset, each
	// such tool starts at the level its hints give it: allow for a tool its
	// server asserts read-only, approve for every other.
	Default MCPToolLevel `yaml:"default,omitempty" json:"default,omitempty" js:"enum=allow|approve|blocked" desc:"The level of every tool levels does not name: allow, approve or blocked. Unset, a tool the server marks read-only runs at allow and every other tool at approve."`

	// Levels sets one tool's level, keyed by the SERVER'S OWN name for it,
	// the one its listing gives before tool_prefix: what an operator reads
	// off the server, and what stays the same when the prefix changes.
	Levels map[string]MCPToolLevel `yaml:"levels,omitempty" json:"levels,omitempty" js:"enum=allow|approve|blocked" desc:"One tool's level, keyed by the server's own name for the tool (before tool_prefix): allow, approve or blocked."`
}

// IsZero lets an entry that sets no level drop out of a round trip: no
// default and no level is the policy an absent block reads as.
func (p MCPToolPolicy) IsZero() bool { return p.Default == "" && len(p.Levels) == 0 }

// Level is the level the tool the server calls raw runs at, and whether the
// entry names it in Levels (custom) rather than leaving it at the default.
// starting is the level the tool's hints start it at (internal/mcp's
// StartingLevel), which an entry with no default leaves it at: the caller
// reads it, since only the server's listing says what a tool's hints are.
func (p MCPToolPolicy) Level(raw string, starting MCPToolLevel) (level MCPToolLevel, custom bool) {
	if l, ok := p.Levels[raw]; ok {
		return l, true
	}
	if p.Default != "" {
		return p.Default, false
	}
	return starting, false
}

// holds reports whether the entry holds the tool the server calls raw at least
// as far as a limit at level would, by its own level or its default. A tool the
// entry leaves to its hints is not held: which level its hints give it is read
// off the server's listing, which no document holds.
func (p MCPToolPolicy) holds(raw string, level org.ToolLimit) bool {
	own, ok := p.Levels[raw]
	if !ok {
		own = p.Default
	}
	switch own {
	case MCPToolBlocked:
		return true
	case MCPToolApprove:
		return level == org.ToolApprove
	}
	return false
}

// AllowsEveryTool reports whether every tool the server lists, whatever it
// lists, runs at [MCPToolAllow]: the default says allow, and no level holds a
// tool back. What an executor box is held to, since a box reaches a server
// whole. An unset default allows only what a server asserts read-only, which
// no document can know before the server lists, so it does not.
func (p MCPToolPolicy) AllowsEveryTool() bool {
	if p.Default != MCPToolAllow {
		return false
	}
	for _, l := range p.Levels {
		if l != MCPToolAllow {
			return false
		}
	}
	return true
}

// heldBack is what keeps a server's tools from all running at allow, in a
// person's words: its default, and each tool a level holds back, sorted.
func (p MCPToolPolicy) heldBack() string {
	var named []string
	for _, tool := range slices.Sorted(maps.Keys(p.Levels)) {
		if l := p.Levels[tool]; l != MCPToolAllow {
			named = append(named, fmt.Sprintf("%s (%s)", tool, l))
		}
	}
	var base string
	switch p.Default {
	case MCPToolAllow:
		return "it sets " + words.And(named)
	case "":
		base = "it sets no default, so each tool it does not name runs at the level its hints " +
			"give it, and a tool the server does not mark read-only asks a person first"
	default:
		base = fmt.Sprintf("its default level is %s", p.Default)
	}
	if len(named) > 0 {
		return base + ", and it sets " + words.And(named)
	}
	return base
}

// validate holds the policy, written at path, to the closed set of levels and
// to tool names a server can list.
func (p MCPToolPolicy) validate(probs *problems, path Path) {
	if p.Default != "" && !p.Default.Valid() {
		probs.add(at(path, "default"), ErrUnknownValue, "%q (want %s)", p.Default, words.Or(MCPToolLevels))
	}
	for _, tool := range slices.Sorted(maps.Keys(p.Levels)) {
		here := entry(at(path, "levels"), tool)
		if level := p.Levels[tool]; !level.Valid() {
			probs.add(here, ErrUnknownValue, "%q (want %s)", level, words.Or(MCPToolLevels))
		}
		toolKey(probs, here, tool)
	}
}

// validateBoxToolLevels refuses an executor box listing of a server whose
// every tool is not certain to run at [MCPToolAllow]
// ([MCPToolPolicy.AllowsEveryTool]): the harness in the box dials the server
// itself, past the engine, so it would reach every tool a level holds back and
// call a tool at approve without anybody's answer.
//
// At the box's entry, whichever of the two was written last: a level written
// on a server some box lists is refused there too, so the person setting it is
// told which box to take the server out of.
//
// An ADMISSION rule: a stored revision that breaks it runs, its box given no
// such server, and warns.
func (c *Company) validateBoxToolLevels() error {
	var p problems
	servers := map[string]*MCPServer{}
	for i := range c.MCPServers {
		servers[c.MCPServers[i].Name] = &c.MCPServers[i]
	}
	for role, path := range c.EachRole() {
		if role.Sandbox == nil {
			continue
		}
		for j, name := range role.Sandbox.MCP.Servers {
			server, declared := servers[name]
			if !declared || server.Tools.AllowsEveryTool() {
				continue
			}
			p.add(idx(at(path, "sandbox.mcp.servers"), j), ErrConflict,
				"%q holds tools back from its agents (%s), and an executor box reaches a "+
					"server whole: the harness in the box dials it itself, past the engine, so "+
					"it would reach the tools held back and call a tool that asks a person "+
					"first without asking. Take %q out of the box, where the seat still reaches "+
					"its tools over the engine's bridge, each at its level, or set its tools to "+
					"`default: allow` with no other level", name, server.Tools.heldBack(), name)
		}
	}
	return p.err()
}
