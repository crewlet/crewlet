package tools

import (
	"context"
	"fmt"
	"testing"
)

// schemaTool carries whatever parameters a case hands it, which is the point:
// for an MCP tool this map came off a third-party server's wire verbatim.
type schemaTool struct {
	name   string
	params map[string]any
}

func (s *schemaTool) Name() string               { return s.name }
func (s *schemaTool) Description() string        { return s.name }
func (s *schemaTool) Parameters() map[string]any { return s.params }
func (s *schemaTool) Call(context.Context, map[string]any) (Result, error) {
	return Result{Output: "ok"}, nil
}

// A TOOL IS ALWAYS OFFERED AN OBJECT SCHEMA, because two consumers require the
// top-level `type: "object"` the MCP spec mandates: a vendor's tool-definition
// API, which rejects the whole request, and the MCP server the bridge builds,
// whose AddTool PANICS. The panic is raised on the turn goroutine that opens
// the bridge, so the wake naks, redelivers and panics again — one
// non-compliant tool in a seat's grant stopped that seat entirely.
//
// Asserted through ToolDefs, the rendering every consumer actually reads.
func TestEveryRenderedToolCarriesAnObjectSchema(t *testing.T) {
	t.Parallel()
	props := map[string]any{"x": map[string]any{"type": "string"}}
	for _, tc := range []struct {
		name       string
		in         map[string]any
		keepsProps bool
	}{
		{"well-formed", map[string]any{"type": "object", "properties": props}, true},
		// The ordinary malformation, and the properties are still good.
		{"no type", map[string]any{"properties": props}, true},
		// This describes something that cannot be an argument object at
		// all, so it is replaced rather than believed.
		{"a type that is not an object", map[string]any{"type": "string"}, false},
		{"nothing at all", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := NewRegistry()
			if err := reg.Register(&schemaTool{name: "t", params: tc.in}, OriginBuiltin); err != nil {
				t.Fatalf("Register: %v", err)
			}
			defs := NewSurface("execute", reg.Snapshot(), []string{"t"}).ToolDefs()
			if len(defs) != 1 {
				t.Fatalf("%d defs", len(defs))
			}
			got := defs[0].Parameters
			if got["type"] != "object" {
				t.Fatalf("type = %v, want object", got["type"])
			}
			if _, ok := got["properties"]; !ok {
				t.Errorf("no properties key: %v", got)
			}
			if kept := fmt.Sprint(got["properties"]) == fmt.Sprint(props); kept != tc.keepsProps {
				t.Errorf("properties kept = %v, want %v (%v)", kept, tc.keepsProps, got)
			}
		})
	}
	// A COPY, still: the rendering must not write a type key back into the
	// registry's own map, which every other seat renders from too.
	in := map[string]any{"properties": props}
	_ = paramsOrEmpty(in)
	if _, mutated := in["type"]; mutated {
		t.Error("the tool's own schema was rewritten in place")
	}
}

// THE REVISION MOVES ONLY WHEN THE ACTIVE SET DOES, which is what lets the MCP
// bridge re-render after an activation and not after the hundreds of ordinary
// bridged calls around it: ToolDefs deep-clones every active tool's schema, so
// asking has to cost less than finding out.
func TestTheSurfaceRevisionTracksRealChanges(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	for _, name := range []string{"a", "b"} {
		if err := reg.Register(&schemaTool{name: name}, OriginBuiltin); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	s := NewSurface("execute", reg.Snapshot(), []string{"a"})
	start := s.Revision()
	if !s.Activate("a") {
		t.Fatal("re-activating a live tool failed")
	}
	if s.Revision() != start {
		t.Error("re-activating a tool already offered moved the revision")
	}
	if s.Activate("nope") {
		t.Fatal("an unknown tool activated")
	}
	if s.Revision() != start {
		t.Error("a refused activation moved the revision")
	}
	if !s.Activate("b") {
		t.Fatal("Activate(b) failed")
	}
	if s.Revision() == start {
		t.Error("a tool joined the surface and the revision did not move")
	}
}
