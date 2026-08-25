package mcp

// THE tool-origin grammar — who put a tool in front of the agents.
//
// It is recorded AT REGISTRATION because it cannot be recovered afterwards. A
// tool an extension registers is structurally identical to one the engine
// ships: same name, same schema, same call signature. With nothing recorded,
// the operator surface called both "builtin", and a tool missing because its
// extension failed to load read as a missing builtin — the one reading that
// sends someone to debug the wrong subsystem.
//
// These strings are a CONTRACT with the operator surface, which groups on
// them, so producer and consumer both derive them here rather than typing the
// literal.
//
// NOTE ON THE HOME OF THIS FILE. The grammar covers four registrants and only
// one of them is MCP. It belongs beside the tool registry, which does not
// exist in the Go tree yet; MCP is the only producer that does. When the
// registry lands this file moves to it wholesale — what must not happen in the
// meantime is a second copy of these four strings appearing next to the
// registry, which is precisely how a grammar stops being one.
const (
	// OriginBuiltin marks a tool the engine itself ships.
	OriginBuiltin = "builtin"

	// OriginCustom marks a tool handed to the engine by an embedding
	// application: not shipped by the engine, not from an extension.
	OriginCustom = "custom"

	// OriginExtensionPrefix prefixes the name of the extension that
	// registered the tool.
	OriginExtensionPrefix = "extension:"

	// OriginMCPPrefix prefixes the BARE name of the MCP server serving the
	// tool — never the per-role instance name. Two seats' children of one
	// template are the same integration to a reader grouping the catalogue,
	// and "mcp:github::Engineer" would split that view per seat.
	OriginMCPPrefix = "mcp:"
)

// ExtensionOrigin is the origin string for a tool an extension registered.
func ExtensionOrigin(extension string) string {
	return OriginExtensionPrefix + extension
}

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
