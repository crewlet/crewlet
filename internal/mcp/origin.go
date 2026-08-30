package mcp

// THE tool-origin grammar — who put a tool in front of the agents.
//
// It is recorded AT REGISTRATION because it cannot be recovered afterwards. A
// tool an MCP server serves is structurally identical to one the engine ships:
// same name, same schema, same call signature. With nothing recorded, the
// operator surface called both "builtin", and a tool missing because its
// server failed to start read as a missing builtin — the one reading that
// sends someone to debug the wrong subsystem.
//
// These strings are a CONTRACT with the operator surface, which groups on
// them, so producer and consumer both derive them here rather than typing the
// literal.
//
// TWO REGISTRANTS, not four. The obvious other two are "custom" (a tool handed
// in by an application embedding the engine) and "extension:<name>" (a
// plugin's), and neither can exist here: nothing under internal/ is
// importable from outside the module, and the engine loads no plugins — it is
// one static binary whose extension point is MCP, out of process. Carrying the
// other two forward would have given the dashboard two groups nothing could
// ever fill, which is how a grammar stops describing the system it names.
//
// WHY THIS LIVES IN mcp AND NOT tools. The registry consumes the grammar, so
// the registry looks like its home — but internal/tools already depends on
// this package for Callable, Result and Annotations, and moving four strings
// the other way would invert that for no gain. internal/tools re-exports them
// exactly as it re-exports those types, which is what keeps ONE definition.
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
