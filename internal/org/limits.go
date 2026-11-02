package org

import (
	"maps"
	"slices"
)

// WHAT A TEAM'S LEAD HOLDS AN MCP SERVER TO, for the agent seats beneath them.
//
// An administrator grants a server on its `mcp_servers` entry (its `access`)
// and sets the level each of its tools runs at there (its `tools`). A seat or
// a unit narrows that with `mcp_limits`, keyed by the server's name:
// `enabled: false` turns the server off for the seat, or for every agent seat
// beneath the unit at any depth, and `tools` holds one tool at `approve` (each
// call waits for a person's answer) or at `blocked` (never offered). Nothing
// written here can widen anything: `enabled: true` is refused where a document
// is written (internal/config), and the block has no word for a grant or for
// `allow`.
//
// `enabled`, THE ENTRY'S OWN WORD, and not `off`: a YAML 1.1 reader (and a
// company document is read by more than this engine) takes the key `off` for
// the boolean false, so `off: true` would reach it as `false: true`.
//
// THE STRICTEST WINS. A seat is held to its own block and to the block of
// every unit it sits in, outward to the top, combined: a server any of them
// turns off is off, and a tool runs at the strictest level any of them names
// ([Role.Limits]), so a team's lead cannot loosen what a lead above them set,
// and neither can a seat's own block. Resolved once, here, as the
// organization is built ([Organization.Normalize]), and combined with the
// server's own entry by the config layer, which every surface that offers,
// starts or reports a server reads.
//
// WRITTEN BY A LEAD as part of their own seats and units (`config.org.write`,
// a subtree write), which the write's admission holds to narrowing: a change
// that lifts or loosens a limit any agent seat is held to is an
// administrator's (internal/config, [config.DiffOrg]'s Widened).

// MCPLimits is a seat's or a unit's limits on MCP servers, keyed by the
// server's name.
type MCPLimits map[string]MCPLimit

// MCPLimit is what one block holds one server to.
type MCPLimit struct {
	// Enabled false turns the server off for the seat, or for every agent
	// seat beneath the unit: it is not started for them, and none of its
	// tools is offered. The grant and every value written for it stay. A
	// limit only narrows, so true says nothing a limit can, and is refused
	// where a document is written.
	Enabled Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero" desc:"false turns this server off for the seat, or for every agent seat beneath the unit; the grant and its values stay. A limit cannot turn a server on, so true is refused."`

	// Tools holds one tool at a level stricter than its own, keyed by the
	// SERVER'S OWN NAME for it (before tool_prefix), as the entry's levels
	// are.
	Tools map[string]ToolLimit `yaml:"tools,omitempty" json:"tools,omitempty" js:"enum=approve|blocked" desc:"One tool held to a stricter level, keyed by the server's own name for it: approve (each call waits for a person's answer) or blocked (never offered)."`
}

// TurnsOff reports a block that turns its server off.
func (l MCPLimit) TurnsOff() bool { return !l.Enabled.Or(true) }

// IsZero reports a block that holds nothing back.
func (l MCPLimit) IsZero() bool { return !l.TurnsOff() && len(l.Tools) == 0 }

// ToolLimit is the level a limit holds a tool to: never `allow`, which only
// the server's own entry sets.
type ToolLimit string

const (
	// ToolApprove holds each call to the tool until a person answers it.
	ToolApprove ToolLimit = "approve"
	// ToolBlocked never offers the tool.
	ToolBlocked ToolLimit = "blocked"
)

// ToolLimits is the closed set, from the least strict.
var ToolLimits = []ToolLimit{ToolApprove, ToolBlocked}

// Valid reports whether l is one of [ToolLimits].
func (l ToolLimit) Valid() bool { return slices.Contains(ToolLimits, l) }

// strictness orders the levels a limit can hold a tool to.
//
// A LEVEL THIS BUILD CANNOT READ IS THE STRICTEST. A limit only ever narrows,
// so a value it cannot name (a stored revision a later build wrote, which the
// write's own rules never admit) holds the tool back as far as any level
// can: read as anything looser, an unread word would hand a seat a tool its
// lead held back.
func (l ToolLimit) strictness() int {
	if l == ToolApprove {
		return 1
	}
	return 2
}

// Stricter reports whether l holds a tool back further than other.
func (l ToolLimit) Stricter(other ToolLimit) bool { return l.strictness() > other.strictness() }

// Clone returns a deep copy: the organization is built from a stored revision
// on every apply, and a block two seats alias would let one's normalisation
// reach the other's.
func (m MCPLimits) Clone() MCPLimits {
	if m == nil {
		return nil
	}
	out := make(MCPLimits, len(m))
	for server, limit := range m {
		out[server] = MCPLimit{Enabled: limit.Enabled, Tools: maps.Clone(limit.Tools)}
	}
	return out
}

// ServerLimit is what every limit on the way down to one agent seat holds one
// server to ([Role.Limits]).
type ServerLimit struct {
	// Off is whether a block turns the server off for the seat; OffBy is the
	// key of the unit whose block does, nearest the seat, or "" for the seat's
	// own.
	Off   bool
	OffBy string

	// Tools is each tool a block holds back, by the server's own name for it.
	Tools map[string]LimitedTool
}

// LimitedTool is the level a tool is held to and the block that holds it there:
// the key of the unit, nearest the seat, or "" for the seat's own.
type LimitedTool struct {
	Level ToolLimit
	By    string
}

// IsZero reports a server nothing limits.
func (s ServerLimit) IsZero() bool { return !s.Off && len(s.Tools) == 0 }

// Tool is the level the limits hold the tool the server calls raw to, and
// whether any does.
func (s ServerLimit) Tool(raw string) (ToolLimit, bool) {
	t, ok := s.Tools[raw]
	return t.Level, ok
}

// LimitOn is what the limits hold the server called name to for this seat:
// nothing for a seat no block limits it on, and always nothing for a human
// seat, which runs no tools.
func (r *Role) LimitOn(name string) ServerLimit {
	if r == nil {
		return ServerLimit{}
	}
	return r.Limits[name]
}

// resolveMCPLimits records on every agent seat what its own `mcp_limits` and
// each enclosing unit's hold it to ([Role.Limits]); a human seat holds none.
//
// EVERY DEPTH, unlike a unit's `mcp_env`: a value is one team's own and stops
// at its members, while a limit is what a lead holds everybody beneath them
// to, so a division's lead turning a server off turns it off for every team in
// the division.
//
// THE NEAREST BLOCK NAMES IT, among those that agree: a seat's own block
// before its unit's, and a unit's before the one enclosing it, so a reader is
// told the most particular place to look.
//
// Recomputed whole on every pass, so a second pass lands on the same answer.
func (o *Organization) resolveMCPLimits() {
	type layer struct {
		by     string
		limits MCPLimits
	}
	hold := func(r *Role, outer []layer) {
		r.Limits = nil
		if !r.IsAgent() {
			return
		}
		layers := append([]layer{{limits: r.MCPLimits}}, outer...)
		for _, l := range layers {
			for _, server := range slices.Sorted(maps.Keys(l.limits)) {
				r.Limits = narrowed(r.Limits, server, l.by, l.limits[server])
			}
		}
	}
	var walk func(u *Unit, outer []layer)
	walk = func(u *Unit, outer []layer) {
		chain := append([]layer{{by: u.Key(), limits: u.MCPLimits}}, outer...)
		for _, r := range u.Roles {
			hold(r, chain)
		}
		for _, c := range u.Children {
			walk(c, chain)
		}
	}
	for _, r := range o.Roles {
		hold(r, nil)
	}
	for _, u := range o.Units {
		walk(u, nil)
	}
}

// narrowed folds one block, written by by, into what limits hold a server to:
// an off stays off and a tool keeps the stricter level, the one already held
// winning a tie, since blocks are folded nearest the seat first. The map is
// the seat's own, built fresh on every pass, so it is written in place.
func narrowed(limits map[string]ServerLimit, server, by string, block MCPLimit) map[string]ServerLimit {
	if block.IsZero() {
		return limits
	}
	if limits == nil {
		limits = map[string]ServerLimit{}
	}
	held := limits[server]
	if block.TurnsOff() && !held.Off {
		held.Off, held.OffBy = true, by
	}
	for _, tool := range slices.Sorted(maps.Keys(block.Tools)) {
		level := block.Tools[tool]
		if !level.Valid() {
			level = ToolBlocked
		}
		if current, ok := held.Tools[tool]; ok && !level.Stricter(current.Level) {
			continue
		}
		if held.Tools == nil {
			held.Tools = map[string]LimitedTool{}
		}
		held.Tools[tool] = LimitedTool{Level: level, By: by}
	}
	limits[server] = held
	return limits
}
