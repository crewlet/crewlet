package queries_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// catalogueTool is a Callable with nothing behind it: these cases are about
// the mapping.
type catalogueTool struct {
	name   string
	schema map[string]any
}

func (f catalogueTool) Name() string               { return f.name }
func (f catalogueTool) Description() string        { return "what " + f.name + " does" }
func (f catalogueTool) Parameters() map[string]any { return f.schema }
func (catalogueTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{}, nil
}

// THE ORIGIN GRAMMAR IS THE REGISTRY'S, passed through rather than re-spelled.
//
// This mapping stripped the `mcp:` prefix and sent the server's bare name, and
// nothing could reach it: every reader of the prefix silently read a company
// running MCP servers as one running none: the tool screen's "from MCP
// servers" count sat at zero beside a chip row listing the servers, and its
// origin column rendered the server's name as if it were the grammar's own
// word. The published API reference documents the prefixed form.
func TestTheCatalogueCarriesTheRegistrysOwnOriginGrammar(t *testing.T) {
	t.Parallel()
	served := queries.CatalogueRow(tools.Entry{
		Tool:   catalogueTool{name: "search"},
		Origin: tools.Origin("tavily"),
	}, "")
	if served.Source != "mcp:tavily" {
		t.Errorf("source = %q, want the prefixed grammar every reader tests for",
			served.Source)
	}
	own := queries.CatalogueRow(tools.Entry{
		Tool: catalogueTool{name: builtin.GetPageTool}, Origin: tools.OriginBuiltin,
	}, "")
	if own.Source != "builtin" {
		t.Errorf("source = %q for the engine's own tool", own.Source)
	}
}

// AND EVERY HINT IS A WORD, including the one nobody advertised.
//
// "The server did not advertise this" and "the server said no" are different
// facts (the whole reason [mcp.Hint] has three values), and a bool cannot
// hold the difference. An absent hint arriving as `false` would read as a
// positive denial, so a fresh server's unannotated tools would present as
// proven reads on the screen an operator audits them on.
func TestAnUnadvertisedHintTravelsAsItself(t *testing.T) {
	t.Parallel()
	got := queries.CatalogueRow(tools.Entry{
		Tool:   catalogueTool{name: "post"},
		Origin: tools.Origin("slack"),
		Annotations: tools.Annotations{
			Title: "Post", ReadOnly: mcp.No, OpenWorld: mcp.Yes,
		},
	}, "slack")

	if got.Annotations.ReadOnly != "no" || got.Annotations.OpenWorld != "yes" {
		t.Errorf("annotations = %+v, want what the registry recorded", got.Annotations)
	}
	if got.Annotations.Destructive != "unknown" || got.Annotations.Idempotent != "unknown" {
		t.Errorf("annotations = %+v, want the unadvertised hints named as unknown",
			got.Annotations)
	}
	if got.Title != "Post" {
		t.Errorf("title = %q", got.Title)
	}
	// WHERE IT LANDS is the registry's own answer, handed in rather than
	// re-derived from the origin: a proven read-only MCP tool delivers
	// nowhere, and Worklet's comment tool delivers although it
	// is a builtin.
	if got.Delivers != "slack" {
		t.Errorf("delivers = %q", got.Delivers)
	}
}

// AND THE SCHEMA IS THE ONE THE MODEL IS OFFERED, not a summary of it.
func TestTheSchemaTravelsWhole(t *testing.T) {
	t.Parallel()
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"key": map[string]any{"type": "string"}},
		"required":   []any{"key"},
	}
	got := queries.CatalogueRow(tools.Entry{
		Tool: catalogueTool{name: "move", schema: schema}, Origin: tools.OriginBuiltin,
	}, "")
	props, _ := got.InputSchema["properties"].(map[string]any)
	if len(props) != 1 {
		t.Errorf("input_schema = %v, want the schema's own field names", got.InputSchema)
	}
	// A tool with no arguments carries nothing rather than an empty object:
	// the wire layer is the one place that distinguishes them, and it keys
	// on the length.
	if bare := queries.CatalogueRow(tools.Entry{
		Tool: catalogueTool{name: "ping"}, Origin: tools.OriginBuiltin,
	}, ""); len(bare.InputSchema) != 0 {
		t.Errorf("input_schema = %v for a tool that takes none", bare.InputSchema)
	}
}

// A BUILT-IN'S ROW SAYS WHERE IT IS FILED AND WHO MAY CALL IT, AND NOTHING
// ELSE'S DOES.
//
// The catalogue's own filing ([builtin.FilingOf]), read for the engine's own
// origin only: an MCP server publishing a tool under a built-in's name is not
// that built-in, and no authority row decides a call to it, so its row
// claiming a gate would tell a reader the engine checks a call it never sees.
// A built-in the catalogue does not file (a phase's own submission) carries
// neither, rather than a guess.
func TestABuiltinsRowNamesItsGroupAndWhoMayCallIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		entry  tools.Entry
		group  builtin.Group
		gate   builtin.Gate
		reason string
	}{
		{"a Worklet read", tools.Entry{
			Tool: catalogueTool{name: builtin.ListWorkItemsTool}, Origin: tools.OriginBuiltin,
		}, builtin.GroupWork, builtin.GateWorklet, "filed with its row's app"},
		{"an artifact share", tools.Entry{
			Tool: catalogueTool{name: builtin.ShareArtifactTool}, Origin: tools.OriginBuiltin,
		}, builtin.GroupArtifacts, builtin.GateArtifactOwner, "filed with its row's role"},
		{"the discovery pair", tools.Entry{
			Tool: catalogueTool{name: toolname.Activate}, Origin: tools.OriginBuiltin,
		}, builtin.GroupDiscovery, builtin.GateEverybody, "no row decides it"},
		{"a server's tool under a built-in's name", tools.Entry{
			Tool: catalogueTool{name: builtin.ListWorkItemsTool}, Origin: tools.Origin("tracker"),
		}, "", "", "not the engine's own"},
		{"a phase's own submission", tools.Entry{
			Tool: catalogueTool{name: toolname.SubmitWork}, Origin: tools.OriginBuiltin,
		}, "", "", "not in the catalogue"},
	} {
		got := queries.CatalogueRow(c.entry, "")
		if got.Group != c.group || got.Gate != c.gate {
			t.Errorf("%s (%s): group %q, gate %q; want %q, %q",
				c.name, c.reason, got.Group, got.Gate, c.group, c.gate)
		}
	}
}

// catalogueBuiltins is a node's built-ins as its runtime hands them over: out
// of the catalogue's order, a shelf with nothing on it (chat), the discovery
// pair, and a built-in the catalogue does not file.
func catalogueBuiltins() []queries.ToolRow {
	row := func(name string, a tools.Annotations, schema map[string]any) queries.ToolRow {
		return queries.CatalogueRow(tools.Entry{
			Tool:        catalogueTool{name: name, schema: schema},
			Origin:      tools.OriginBuiltin,
			Annotations: a,
		}, "")
	}
	read := tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}
	return []queries.ToolRow{
		row(builtin.CreateWorkItemTool, tools.Annotations{Title: "File a work item",
			ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes},
			map[string]any{"type": "object"}),
		row(toolname.SubmitWork, tools.Annotations{}, nil),
		row(builtin.ShareArtifactTool, tools.Annotations{}, nil),
		row(builtin.ListWorkItemsTool, read, nil),
		row(toolname.Activate, tools.Annotations{}, nil),
	}
}

// THE BUILT-INS ARE SHELVED IN THE CATALOGUE'S ORDER, AND ONLY WHAT A SEAT IS
// OFFERED.
//
// The MCP page draws one rail entry per shelf and the shelf's tools in the
// order the engine files them, so the answer is in that order whatever order
// the registry handed them over in. A shelf this company offers nothing of is
// left out (an empty "Chat" on a company running Slack would say its agents
// hold a group they do not), and a built-in the catalogue does not file has
// no shelf to sit on.
func TestTheBuiltinsAreShelvedInTheCataloguesOrder(t *testing.T) {
	t.Parallel()
	got := askMCPStatus(t, mcpSources(t, mcpFleet(t))).Builtin

	var shelves []builtin.Group
	for _, g := range got {
		shelves = append(shelves, g.Key)
		if g.Label != g.Key.Label() || g.Description != g.Key.Description() {
			t.Errorf("shelf %q is worded %q, %q; the engine's words are %q, %q",
				g.Key, g.Label, g.Description, g.Key.Label(), g.Key.Description())
		}
	}
	want := []builtin.Group{builtin.GroupWork, builtin.GroupArtifacts, builtin.GroupDiscovery}
	if !slices.Equal(shelves, want) {
		t.Fatalf("shelves = %v, want %v: the catalogue's order, empty ones left out",
			shelves, want)
	}
	var work []string
	for _, row := range got[0].Tools {
		work = append(work, row.Name)
	}
	if want := []string{builtin.ListWorkItemsTool, builtin.CreateWorkItemTool}; !slices.Equal(work, want) {
		t.Errorf("work items = %v, want %v: the catalogue's own order", work, want)
	}
	for _, g := range got {
		for _, row := range g.Tools {
			if row.Name == toolname.SubmitWork {
				t.Errorf("%s is on shelf %q, and the catalogue files it nowhere",
					row.Name, g.Key)
			}
			if row.Gate == "" {
				t.Errorf("%s is shelved and says nothing about who may call it", row.Name)
			}
		}
	}

	// AND A NODE WITH NO REVISION SENDS AN EMPTY LIST, never a null.
	s := mcpSources(t, mcpFleet(t))
	s.Builtins = func() []queries.ToolRow { return nil }
	if body := asMap(t, askMCPStatus(t, s)); body["builtin"] == nil {
		t.Error("builtin = null; a client reads that as a build that does not send it")
	}
}

// THE TOOL SCREENS READ EXACTLY WHAT THE CATALOGUE SENDS, every field held
// both ways and every closed word set the engine's own.
//
// One row shape for `GET /tools`, the `tools` push and the MCP answer's
// shelves ([queries.ToolRow]), so one gate: the rows below are what
// [queries.CatalogueRow] builds, and the push marshals the same struct. The
// dashboard declares them in `contract/tools.ts`: ToolRow, ToolAnnotations,
// and the ToolHint, BuiltinGroupKey and BuiltinGate unions.
func TestTheCatalogueIsWhatTheToolScreensRead(t *testing.T) {
	t.Parallel()
	served := queries.CatalogueRow(tools.Entry{
		Tool: catalogueTool{name: "create_issue"}, Origin: tools.Origin("github"),
	}, "github")
	rows := []map[string]any{asMap(t, served)}
	for _, row := range catalogueBuiltins() {
		rows = append(rows, asMap(t, row))
	}
	holdShape(t, "ToolRow", rows, false)
	var hints []map[string]any
	for _, row := range rows {
		hints = append(hints, asMap(t, row["annotations"]))
	}
	holdShape(t, "ToolAnnotations", hints, false)

	words := []string{mcp.Unknown.String(), mcp.Yes.String(), mcp.No.String()}
	for _, c := range []struct {
		union string
		want  []string
	}{
		{"ToolHint", words},
		{"BuiltinGroupKey", stringsOf(builtin.Groups)},
		{"BuiltinGate", stringsOf(builtin.Gates)},
	} {
		got, err := clientsource.Union(clientsource.Tree(t), c.union)
		if err != nil {
			t.Fatalf("%v: this gate cannot run without the client's declaration", err)
		}
		slices.Sort(got)
		want := slices.Sorted(slices.Values(c.want))
		if !slices.Equal(got, want) {
			t.Errorf("the dashboard's %s is %v; the engine sends %v", c.union, got, want)
		}
	}
}
