package toolschema_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tools/toolschema"
	"github.com/crewlet/crewlet/internal/tools/toolschema/toolschematest"
)

// AN OBJECT WITH NOTHING REQUIRED LEAVES `required` OUT: a nil list marshals
// as null, which Claude Code refuses, dropping the server's every tool.
//
// Mutation: set "required" unconditionally in Object and this goes red.
func TestAnObjectWithNothingRequiredLeavesRequiredOut(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(toolschema.Object(map[string]any{"q": map[string]any{"type": "string"}}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "required") {
		t.Errorf("an object requiring nothing marshals %s", raw)
	}
	req := toolschema.Object(map[string]any{"q": map[string]any{"type": "string"}}, "q")
	if faults := toolschematest.Faults(req); len(faults) > 0 {
		t.Errorf("an object requiring its property has faults: %v", faults)
	}
}

// FAULTS FINDS WHAT MAKES A CLIENT REFUSE A TOOL, and passes what JSON Schema
// allows: a null where an instance value goes, a boolean subschema, a
// required name an open object may hold, as the draft 2020-12 meta-schema
// does.
func TestFaultsFindsWhatMakesAClientRefuseATool(t *testing.T) {
	t.Parallel()
	str := map[string]any{"type": "string"}
	for name, tc := range map[string]struct {
		schema map[string]any
		want   string
	}{
		"a valid object": {schema: map[string]any{"type": "object", "properties": map[string]any{"q": str},
			"required": []string{"q"}}},
		"nulls as instance values": {schema: map[string]any{"type": "object", "properties": map[string]any{
			"q": map[string]any{"type": []string{"string", "null"}, "default": nil, "enum": []any{"a", nil}}}}},
		"a boolean subschema": {schema: map[string]any{"type": "object", "additionalProperties": true}},
		"an open object requiring an undeclared member": {schema: map[string]any{"type": "object",
			"required": []string{"extra"}}},
		"a nil required list": {schema: map[string]any{"type": "object", "properties": map[string]any{},
			"required": []string(nil)}, want: "required is null"},
		"a required string": {schema: map[string]any{"type": "object", "required": "q"},
			want: "required is a string, not a list"},
		"a required number": {schema: map[string]any{"type": "object", "required": []any{1}},
			want: "required[0] is a number, not a string"},
		"a name required twice": {schema: map[string]any{"type": "object", "properties": map[string]any{"q": str},
			"required": []string{"q", "q"}}, want: `names "q" twice`},
		"a closed object requiring what it does not declare": {schema: toolschema.Object(
			map[string]any{"q": str}, "r"), want: `names "r", which is not a property`},
		"a nested nil required list": {schema: map[string]any{"type": "object", "properties": map[string]any{
			"o": map[string]any{"type": "object", "required": []string(nil)}}}, want: "properties.o.required is null"},
		"a null item schema": {schema: map[string]any{"type": "object", "properties": map[string]any{
			"l": map[string]any{"type": "array", "items": nil}}}, want: "properties.l.items is null"},
		"a null description": {schema: map[string]any{"type": "object", "description": nil},
			want: "description is null"},
		"a property that is not a schema": {schema: map[string]any{"type": "object", "properties": map[string]any{
			"q": "string"}}, want: "properties.q is a string, not a schema"},
		"a list type": {schema: map[string]any{"type": "array"}, want: `type is array, not "object"`},
		"no type":     {schema: map[string]any{"properties": map[string]any{}}, want: `type is <nil>, not "object"`},
		"an enum that is no list": {schema: map[string]any{"type": "object", "properties": map[string]any{
			"q": map[string]any{"enum": "a"}}}, want: "properties.q.enum is a string, not a list"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			faults := toolschema.Faults(tc.schema)
			if tc.want == "" {
				faults = toolschematest.Faults(tc.schema)
			}
			got := strings.Join(faults, "; ")
			switch {
			case tc.want == "" && len(faults) > 0:
				t.Errorf("faults %q", got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("faults %q, want one saying %q", got, tc.want)
			}
		})
	}
}
