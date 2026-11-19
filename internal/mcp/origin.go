package mcp

import "strings"

// THE tool-origin grammar — who put a tool in front of the agents.
//
// Recorded AT REGISTRATION, as it cannot be recovered: a tool missing because
// its MCP server failed to start would read as a missing builtin. The operator
// surface groups on these strings, so both sides derive them here.
//
// TWO REGISTRANTS: no plugins load (the extension point is MCP), so no
// "custom" or "extension:<name>" origin can exist.
//
// IN mcp, NOT tools: internal/tools already depends on this package for
// Callable, Result and Annotations, and re-exports these as it re-exports
// those, keeping ONE definition.
const (
	// OriginBuiltin marks a tool the engine itself ships. The
	// agent-to-agent tools are builtins too: a2a_ask is registered by the
	// same walk, so "a2a" is a capability rather than an origin.
	OriginBuiltin = "builtin"

	// OriginMCPPrefix prefixes the BARE name of the MCP server serving the
	// tool — never the per-role instance name. Two seats' children of one
	// template are the same integration to a reader grouping the catalogue,
	// and "mcp:github::Engineer" would split that view per seat.
	OriginMCPPrefix = "mcp:"
)

// Origin is the origin string for a tool served by an MCP server. Pass the
// bare server name; ServerName turns an instance name into one.
func Origin(server string) string {
	return OriginMCPPrefix + server
}

// OriginServer is the bare MCP server an origin names, and false for one that
// names none: a builtin, or no origin at all.
//
// THE EMPTY NAME ON A MISS IS THE CONTRACT: [strings.CutPrefix] hands back its
// whole input when the prefix is absent, and a reader that took that unchecked
// would file every builtin under a server called `builtin`.
func OriginServer(origin string) (string, bool) {
	server, ok := strings.CutPrefix(origin, OriginMCPPrefix)
	if !ok || server == "" {
		return "", false
	}
	return server, true
}

// Registration is a tool together with WHO put it in front of the agents.
//
// It exists so the origin cannot be lost between this package and whatever
// registry consumes it: the bridge does not hand out a bare tool for
// registration, it hands out this, and the registry's register call takes the
// pair. The registration call is the only frame that knows the answer, and it
// is the last one that can record it.
type Registration struct {
	Tool   Callable
	Origin string
}
