package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/words"
)

// EACH TOOL OF AN MCP SERVER AT A LEVEL, set on its entry.
//
// `tools` on an `mcp_servers` entry sets the level each of the server's tools
// runs at: `levels` names a tool by the server's own name (the one its listing
// gives, before `tool_prefix`), and `default` is the level of every tool
// `levels` does not name. A tool at `allow` is offered to every agent the
// server reaches; a tool at `blocked` is never filed, so no agent is offered
// it, lists it or can call it, on the engine host or from an executor's box.
//
// ENFORCED WHERE THE TOOLS ARE FILED: the engine hands the levels to the MCP
// client with the server's start (internal/mcp, `Spec.Levels`), which leaves a
// blocked tool out of what the server contributes, so every reader of a seat's
// surface (the tool loop on the engine host, the bridge an executor dials, the
// discovery tools) sees the same set and none of them can be asked for a tool
// the others leave out.
//
// AN EXECUTOR BOX REACHES A SERVER WHOLE: a server listed in a role's
// `sandbox.mcp.servers` is dialled by the harness inside the box, past the
// engine, so a box cannot leave one tool out. Such a listing is refused for a
// server with any tool at a level other than `allow`
// ([Company.validateBoxToolLevels]), and the engine gives no box such a server
// whatever a stored revision says. The seat still reaches the server's allowed
// tools over the bridge.

// MCPToolLevel is the level a tool of an MCP server runs at.
type MCPToolLevel string

const (
	// MCPToolAllow offers the tool to every agent the server reaches.
	MCPToolAllow MCPToolLevel = "allow"
	// MCPToolBlocked never files the tool: no agent is offered it, lists it
	// or can call it.
	MCPToolBlocked MCPToolLevel = "blocked"
)

// MCPToolLevels is the closed set, in the order a reader is told them.
var MCPToolLevels = []MCPToolLevel{MCPToolAllow, MCPToolBlocked}

// Valid reports whether l is one of [MCPToolLevels].
func (l MCPToolLevel) Valid() bool { return slices.Contains(MCPToolLevels, l) }

// MCPToolPolicy is the level of each of a server's tools ([MCPServer.Tools]).
type MCPToolPolicy struct {
	// Default is the level of every tool Levels names none for. Unset is
	// [MCPToolAllow]: a server whose entry says nothing offers every tool
	// it lists.
	Default MCPToolLevel `yaml:"default,omitempty" json:"default,omitempty" desc:"The level of every tool levels does not name: allow (the default) or blocked."`

	// Levels sets one tool's level, keyed by the SERVER'S OWN name for it,
	// the one its listing gives before tool_prefix: what an operator reads
	// off the server, and what stays the same when the prefix changes.
	Levels map[string]MCPToolLevel `yaml:"levels,omitempty" json:"levels,omitempty" desc:"One tool's level, keyed by the server's own name for the tool (before tool_prefix): allow or blocked."`
}

// IsZero lets an entry that sets no level drop out of a round trip: no
// default and no level is the policy an absent block reads as.
func (p MCPToolPolicy) IsZero() bool { return p.Default == "" && len(p.Levels) == 0 }

// DefaultLevel is the level of a tool Levels names none for, applying the
// allow default.
func (p MCPToolPolicy) DefaultLevel() MCPToolLevel {
	if p.Default == "" {
		return MCPToolAllow
	}
	return p.Default
}

// Level is the level the tool the server calls raw runs at, and whether the
// entry names it in Levels (custom) rather than leaving it at the default.
func (p MCPToolPolicy) Level(raw string) (level MCPToolLevel, custom bool) {
	if l, ok := p.Levels[raw]; ok {
		return l, true
	}
	return p.DefaultLevel(), false
}

// AllowsEveryTool reports whether every tool the server lists, whatever it
// lists, runs at [MCPToolAllow]: the default allows, and no level holds one
// back. What an executor box is held to, since a box reaches a server whole.
func (p MCPToolPolicy) AllowsEveryTool() bool {
	if p.DefaultLevel() != MCPToolAllow {
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
	switch {
	case p.DefaultLevel() != MCPToolAllow && len(named) > 0:
		return fmt.Sprintf("its default level is %s, and it sets %s", p.DefaultLevel(), words.And(named))
	case p.DefaultLevel() != MCPToolAllow:
		return fmt.Sprintf("its default level is %s", p.DefaultLevel())
	}
	return "it sets " + words.And(named)
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
		// A NAME NO SERVER LISTS is refused where it can be told from the
		// name alone: nothing, or a name holding a space or a control
		// character, which no tool name carries. A well-formed name the
		// server does not list is a fact about the server, not the
		// document, and is reported beside the listing instead.
		if strings.TrimSpace(tool) == "" {
			probs.add(here, ErrMissing, "an empty key names no tool: key a level by the "+
				"server's own name for the tool")
		} else if strings.IndexFunc(tool, unicode.IsSpace) >= 0 || strings.IndexFunc(tool, unicode.IsControl) >= 0 {
			probs.add(here, ErrUnknownValue, "%q holds a space or a control character, which "+
				"no tool name does: key a level by the server's own name for the tool", tool)
		}
	}
}

// validateBoxToolLevels refuses an executor box listing of a server with any
// tool at a level other than [MCPToolAllow]: the harness in the box dials the
// server itself, past the engine, so it would reach every tool the level holds
// back.
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
					"it would reach the tools held back. Take %q out of the box, where the seat "+
					"still reaches its allowed tools over the engine's bridge, or set every one "+
					"of its tools to allow", name, server.Tools.heldBack(), name)
		}
	}
	return p.err()
}
