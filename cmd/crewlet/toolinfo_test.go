package main

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// fakeTool is a Callable with nothing behind it: this is about the mapping.
type fakeTool struct {
	name   string
	schema map[string]any
}

func (f fakeTool) Name() string               { return f.name }
func (f fakeTool) Description() string        { return "what " + f.name + " does" }
func (f fakeTool) Parameters() map[string]any { return f.schema }
func (fakeTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{}, nil
}

// THE ORIGIN GRAMMAR IS THE REGISTRY'S, passed through rather than re-spelled.
//
// This mapping stripped the `mcp:` prefix and sent the server's bare name, and
// nothing could reach it: every reader of the prefix silently read a company
// running MCP servers as one running none — the tool screen's "from MCP
// servers" count sat at zero beside a chip row listing the servers, and its
// origin column rendered the server's name as if it were the grammar's own
// word. The published API reference documents the prefixed form.
func TestTheCatalogueCarriesTheRegistrysOwnOriginGrammar(t *testing.T) {
	t.Parallel()
	served := toolInfo(tools.Entry{
		Tool:   fakeTool{name: "search"},
		Origin: tools.Origin("tavily"),
	}, "")
	if served.Source != "mcp:tavily" {
		t.Errorf("source = %q, want the prefixed grammar every reader tests for",
			served.Source)
	}
	own := toolInfo(tools.Entry{Tool: fakeTool{name: "get_page"}, Origin: tools.OriginBuiltin}, "")
	if own.Source != "builtin" {
		t.Errorf("source = %q for the engine's own tool", own.Source)
	}
}

// AND EVERY HINT IS A WORD, including the one nobody advertised.
//
// "The server did not advertise this" and "the server said no" are different
// facts — the whole reason [mcp.Hint] has three values — and a bool cannot
// hold the difference. An absent hint arriving as `false` would read as a
// positive denial, so a fresh server's unannotated tools would present as
// proven reads on the screen an operator audits them on.
func TestAnUnadvertisedHintTravelsAsItself(t *testing.T) {
	t.Parallel()
	got := toolInfo(tools.Entry{
		Tool:   fakeTool{name: "post"},
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
	if got.Annotations.Title != "Post" {
		t.Errorf("title = %q", got.Annotations.Title)
	}
	// WHERE IT LANDS is the registry's own answer, handed in rather than
	// re-derived from the origin: a proven read-only MCP tool delivers
	// nowhere, and the native tracker's comment tool delivers although it
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
	got := toolInfo(tools.Entry{
		Tool: fakeTool{name: "move", schema: schema}, Origin: tools.OriginBuiltin,
	}, "")
	props, _ := got.InputSchema["properties"].(map[string]any)
	if len(props) != 1 {
		t.Errorf("input_schema = %v, want the schema's own field names", got.InputSchema)
	}
	// A tool with no arguments carries nothing rather than an empty object:
	// the wire layer is the one place that distinguishes them, and it keys
	// on the length.
	if bare := toolInfo(tools.Entry{
		Tool: fakeTool{name: "ping"}, Origin: tools.OriginBuiltin,
	}, ""); len(bare.InputSchema) != 0 {
		t.Errorf("input_schema = %v for a tool that takes none", bare.InputSchema)
	}
}
