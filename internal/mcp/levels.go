package mcp

import "maps"

// Level is the level a tool of a server runs at, as the server's entry sets
// it (internal/config's `mcp_servers[].tools`).
type Level string

const (
	// LevelAllow files the tool: it is offered, listed and called like any
	// other.
	LevelAllow Level = "allow"
	// LevelBlocked never files the tool. It is in no catalogue, so no model
	// is offered it or lists it, and no call through a catalogue can reach
	// it; the bridge keeps its definition apart ([Bridge.BlockedTools]) so a
	// reader of the server's listing sees what was held back.
	LevelBlocked Level = "blocked"
)

// Levels is the level of each tool a server lists ([Spec.Levels]).
//
// BY THE SERVER'S OWN NAME, before [Spec.ToolPrefix], as the operator reads it
// off the server's listing; and a DEFAULT, because a server that adds a tool
// next week adds it at the level the operator chose for the tools they had not
// named, which a list of names to skip could not say before the server listed
// them.
type Levels struct {
	// Default is the level of every tool Tools names none for; "" is
	// [LevelAllow].
	Default Level
	// Tools sets one tool's level by the server's own name for it.
	Tools map[string]Level
}

// Of is the level of the tool the server calls raw: its own, else the default.
func (l Levels) Of(raw string) Level {
	if level, ok := l.Tools[raw]; ok {
		return level
	}
	return l.defaultLevel()
}

// Blocks reports whether the tool the server calls raw is never filed.
func (l Levels) Blocks(raw string) bool { return l.Of(raw) == LevelBlocked }

func (l Levels) defaultLevel() Level {
	if l.Default == "" {
		return LevelAllow
	}
	return l.Default
}

// same reports whether two policies decide every tool alike: an unset default
// is an allowing one, and no levels at all are an empty set of them.
func (l Levels) same(other Levels) bool {
	return l.defaultLevel() == other.defaultLevel() && maps.Equal(l.Tools, other.Tools)
}
