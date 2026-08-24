package mcp

import (
	"context"
	"fmt"
	"maps"
)

// Result is what one tool call produced.
//
// There is ONE output field, not an output-plus-error pair, and that is
// deliberate: the text goes back to the model either way, and splitting it
// into two fields is how a Python incident happened — the upstream error was
// written to `error` while the loop rendered `output`, so the agent was shown
// a generic "tool execution failed" instead of the server's actual reason. It
// maps onto toolloop.ToolResult field for field.
type Result struct {
	// Output is fed back to the model as the tool message's content.
	Output string

	// Failed marks a tool that reported failure. The output still goes back —
	// that is the point — but a reader can tell a tool that ran from one that
	// refused.
	Failed bool
}

// Callable is the least a phase surface needs to offer something to a model
// and run what it asked for. Both a bridged MCP tool and a discovery meta-tool
// satisfy it.
type Callable interface {
	Name() string
	Description() string
	// Parameters is a JSON Schema object.
	Parameters() map[string]any
	// Call runs the tool. An ERROR means the caller's own context ended —
	// the turn is being torn down — and nothing should be reported to the
	// model. A tool that failed is an ordinary Result with Failed set.
	Call(ctx context.Context, args map[string]any) (Result, error)
}

// Tool is one MCP server tool, wrapped for the engine.
type Tool struct {
	name        string // the catalogue name: prefix applied
	raw         string // the name the server knows it by
	description string
	parameters  map[string]any
	annotations Annotations
	client      *client
}

var _ Callable = (*Tool)(nil)

func newTool(c *client, def toolDef, spec Spec) *Tool {
	name := spec.ToolPrefix + def.Name

	// An override may be keyed by EITHER name. The raw one is what the server
	// reports; the prefixed one is what the operator sees in the catalogue
	// and the dashboard, and keying by that must not silently do nothing.
	// Raw first, so the more specific spelling wins if both are present.
	ann := def.Annotations
	if override, ok := spec.AnnotationOverrides[def.Name]; ok {
		ann = ann.Merge(override)
	} else if override, ok := spec.AnnotationOverrides[name]; ok {
		ann = ann.Merge(override)
	}

	return &Tool{
		name:        name,
		raw:         def.Name,
		description: def.Description,
		parameters:  maps.Clone(def.InputSchema),
		annotations: ann,
		client:      c,
	}
}

// Name is the catalogue name, with the server's tool_prefix applied.
func (t *Tool) Name() string { return t.name }

// RawName is the name the server itself uses, which is what goes on the wire.
func (t *Tool) RawName() string { return t.raw }

// Description is what the model is told the tool does.
func (t *Tool) Description() string { return t.description }

// Parameters is the tool's JSON Schema, as the server published it.
func (t *Tool) Parameters() map[string]any { return t.parameters }

// Annotations are the server's behavioural hints with operator overrides
// applied. Tri-state: see annotations.go.
func (t *Tool) Annotations() Annotations { return t.annotations }

// Instance is the full instance name of the server serving this tool —
// "github::Engineer" for a per-role child. It identifies the PROCESS.
func (t *Tool) Instance() string { return t.client.name }

// Server is the bare server name, which is what the model is shown and what it
// types into list_mcp_server_tools. Two seats' children of one template are
// the same server to a reader.
func (t *Tool) Server() string { return ServerName(t.client.name) }

// Origin is this tool's entry in the tool-origin grammar, ready for whatever
// registry the caller keeps. See origin.go.
func (t *Tool) Origin() string { return MCPOrigin(t.Server()) }

// Call runs the tool on its server.
//
// A failing tool is ORDINARY and comes back as a Result with Failed set: the
// server's own words go to the model, which is expected to react to them. The
// error return is reserved for the caller's context ending, because a turn
// that is being torn down has nobody left to show a tool message to — and
// because reporting a cancellation as a tool failure would teach the model
// that the tool is broken.
func (t *Tool) Call(ctx context.Context, args map[string]any) (Result, error) {
	blocks, err := t.client.callTool(ctx, t.raw, args)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, err
		}
		return Result{
			Output: fmt.Sprintf("MCP tool error (%s/%s): %v", t.client.name, t.raw, err),
			Failed: true,
		}, nil
	}
	out := renderBlocks(blocks)
	if out == "" {
		// A server that answers with nothing at all is not an error, but a
		// blank tool message reads to a model as a dropped turn.
		out = "Tool returned no output."
	}
	return Result{Output: out}, nil
}

// Entry is one tool as the discovery meta-tools see it: enough to bucket it by
// server and describe it, and nothing that would let them call it.
type Entry struct {
	Name        string
	Description string
	// Server is the bare server name, or "" for a tool that is not an MCP
	// tool at all — a builtin in the merged catalogue.
	Server string
}

// EntriesOf projects bridged tools into catalogue entries.
func EntriesOf(tools []*Tool) []Entry {
	out := make([]Entry, len(tools))
	for i, t := range tools {
		out[i] = Entry{Name: t.Name(), Description: t.Description(), Server: t.Server()}
	}
	return out
}
