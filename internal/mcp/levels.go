package mcp

import (
	"maps"
	"slices"
)

// Level is the level a tool of a server runs at, as the server's entry sets
// it (internal/config's `mcp_servers[].tools`), or as its hints start it
// ([StartingLevel]).
type Level string

const (
	// LevelAllow files the tool: it is offered, listed and called like any
	// other.
	LevelAllow Level = "allow"
	// LevelApprove files the tool, and asks a person before each call. It
	// is offered and listed like any other, and a call to it never runs
	// until a person answers: an executor's run records the call on its
	// row and parks on the answer, and a phase on the engine host, which
	// has no run to park, is refused the call (internal/tools'
	// [Surface], ADR-0077).
	LevelApprove Level = "approve"
	// LevelBlocked never files the tool. It is in no catalogue, so no model
	// is offered it or lists it, and no call through a catalogue can reach
	// it; the bridge keeps its definition apart ([Bridge.BlockedTools]) so a
	// reader of the server's listing sees what was held back.
	LevelBlocked Level = "blocked"
)

// AllLevels is every level, in the order a reader is told them, for the gate
// holding a reader's copy.
var AllLevels = []Level{LevelAllow, LevelApprove, LevelBlocked}

// Valid reports whether l is a level this build enforces.
func (l Level) Valid() bool { return slices.Contains(AllLevels, l) }

// Levels is the level of each tool a server lists ([Spec.Levels]).
//
// BY THE SERVER'S OWN NAME, before [Spec.ToolPrefix], as the operator reads it
// off the server's listing; and a DEFAULT, because a server that adds a tool
// next week adds it at the level the operator chose for the tools they had not
// named, which a list of names to skip could not say before the server listed
// them.
type Levels struct {
	// Default is the level of every tool Tools names none for; "" leaves
	// each such tool at its starting level, read from its hints
	// ([StartingLevel]).
	Default Level
	// Tools sets one tool's level by the server's own name for it.
	Tools map[string]Level
}

// Of is the level of the tool the server calls raw, whose hints (the
// operator's overrides applied) are hints: its own, else the default, else the
// level its hints start it at.
func (l Levels) Of(raw string, hints Annotations) Level {
	if level, ok := l.Tools[raw]; ok {
		return level
	}
	if l.Default != "" {
		return l.Default
	}
	return StartingLevel(RiskOf(hints))
}

// Blocks reports whether the tool the server calls raw is never filed: a
// level the entry sets, by the tool's name or as the default, since no hint
// starts a tool blocked.
func (l Levels) Blocks(raw string) bool {
	if level, ok := l.Tools[raw]; ok {
		return level == LevelBlocked
	}
	return l.Default == LevelBlocked
}

// same reports whether two policies decide every tool alike: the same default,
// an unset one included (it leaves each tool at its starting level), and the
// same levels, no levels at all being an empty set of them.
func (l Levels) same(other Levels) bool {
	return l.Default == other.Default && maps.Equal(l.Tools, other.Tools)
}
