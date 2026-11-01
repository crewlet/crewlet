// One tool as the catalogue carries it.

package queries

import (
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/tools"
)

// ToolRow is one tool on the wire: a row of `GET /tools` and the `tools` push,
// and of `mcp_servers_status`'s built-in groups.
//
// ONE SHAPE FOR EVERY ANSWER THAT LISTS A TOOL, built by one function
// ([CatalogueRow]), so the catalogue and the MCP page cannot describe one tool
// two ways. The field names are the CLIENT's, because the dashboard is the
// compatibility reference for a frame's shape and it groups the tool screen
// by `source`.
type ToolRow struct {
	Name        string `json:"name"`
	Description string `json:"description"`

	// Source is the registry's ORIGIN GRAMMAR verbatim (`builtin`, or
	// `mcp:` and the bare server name), which is what the tool screen
	// groups on and what the API reference documents.
	//
	// The prefix is load-bearing and was once stripped here: a reader
	// cannot tell a server called `builtin` from the engine's own tools
	// without it, and every consumer that tests for it (the screen's
	// origin column, its per-origin counts) silently read a company
	// running MCP servers as one running none.
	Source string `json:"source"`

	// Title is the human-readable name a server advertised, omitted when
	// it advertised none.
	Title string `json:"title,omitempty"`

	// Annotations are the behavioural hints recorded at REGISTRATION:
	// what the engine's own delivery fence, the operator MCP surface and
	// the sandbox bridge all read, and what the tool screen could not show
	// because the catalogue on the wire carried three strings.
	//
	// It is the difference between a list of names and a surface an
	// operator can audit: which of a fresh server's tools can write, which
	// can write IRREVERSIBLY, and which reach outside the company at all.
	Annotations ToolHints `json:"annotations"`

	// Delivers names WHERE calling this tool puts something in front of
	// somebody outside the turn, and is empty for a tool that reaches
	// nobody. The registry's own predicate, never re-derived here: a tool
	// that delivers through the fence and not on the screen is the drift
	// this field exists to make visible.
	Delivers string `json:"delivers"`

	// InputSchema is the tool's JSON Schema, exactly as the model is
	// offered it. Carried whole rather than summarised, because the screen
	// that renders "how would I call this" substitutes an object's ids
	// into the schema's OWN field names, and a summary would be a second,
	// drifting description of the one thing the engine already states.
	InputSchema map[string]any `json:"input_schema,omitempty"`

	// Group is the shelf of the built-in catalogue the tool is filed on,
	// and Gate who may call it: what its authority row asks of the caller
	// ([builtin.FilingOf]). HOLDING A TOOL IS NOT BEING ALLOWED TO CALL
	// IT, and a reader auditing what an agent holds needs both.
	//
	// EMPTY, AND PRESENT, on a tool the catalogue does not file (an MCP
	// server's, whose calls no authority row decides), as [ToolRow.Delivers]
	// is on a tool that reaches nobody: left out, a client could not tell
	// "not one of the engine's own" from a build that does not send it.
	Group builtin.Group `json:"group"`
	Gate  builtin.Gate  `json:"gate"`
}

// ToolHints is one tool's behavioural hints on the wire.
//
// EVERY HINT IS THREE-VALUED and rendered as a word, never as a bool. "The
// server did not advertise this" and "the server said no" are different facts
// (the whole reason [mcp.Hint] exists), and a JSON bool cannot hold the
// difference: an absent hint would arrive as `false` and read as a positive
// denial, which is exactly how a fresh MCP server's unannotated tools would
// come to look like proven reads on the one screen an operator audits them on.
//
// Words rather than `true | false | null` for the client's sake: all three
// values are truthy, so a careless `if (!ann.read_only)` cannot silently mean
// "not read-only" for a tool nobody annotated.
type ToolHints struct {
	ReadOnly    string `json:"read_only"`
	Destructive string `json:"destructive"`
	Idempotent  string `json:"idempotent"`
	OpenWorld   string `json:"open_world"`
}

// CatalogueRow is one registry entry as the catalogue carries it, with the
// surface a call to it delivers to.
//
// The mapping had a defect nothing could reach while it lived inside the loop
// that read the registry: it re-spelled the origin instead of passing the
// registry's own, and every reader of the prefix (the screen's origin column,
// its per-origin counts, the published reference) silently read a company
// running MCP servers as one running none.
func CatalogueRow(entry tools.Entry, delivers string) ToolRow {
	row := ToolRow{
		Name:        entry.Name(),
		Description: entry.Tool.Description(),
		// THE REGISTRY'S OWN GRAMMAR, passed through rather than
		// re-spelled: `builtin`, or `mcp:` and the bare server name.
		Source: entry.Origin,
		Title:  entry.Annotations.Title,
		Annotations: ToolHints{
			ReadOnly:    entry.Annotations.ReadOnly.String(),
			Destructive: entry.Annotations.Destructive.String(),
			Idempotent:  entry.Annotations.Idempotent.String(),
			OpenWorld:   entry.Annotations.OpenWorld.String(),
		},
		Delivers: delivers,
		// The schema the MODEL is offered, read rather than rebuilt. The
		// map is the tool's own and this path only serialises it; an MCP
		// server restarting replaces the whole Tool rather than writing
		// into its schema, so there is nothing here for a running turn to
		// race with.
		InputSchema: entry.Tool.Parameters(),
	}
	// FILED ONLY FROM THE ENGINE'S OWN ORIGIN: an MCP server may publish a
	// tool under a built-in's name, and the registry refuses the second
	// registrant, but a row is built from whatever entry it is handed.
	if entry.Origin == tools.OriginBuiltin {
		if f, ok := builtin.FilingOf(entry.Name()); ok {
			row.Group, row.Gate = f.Group, f.Gate
		}
	}
	return row
}
