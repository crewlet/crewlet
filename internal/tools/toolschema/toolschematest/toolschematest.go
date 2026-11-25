// Package toolschematest holds a tool's input schema to what an MCP client
// can take, for the sweeps over every first-party tool: [toolschema.Faults],
// and the JSON Schema draft 2020-12 meta-schema, which links a validator
// into tests alone.
package toolschematest

import (
	"bytes"
	"encoding/json"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/crewlet/crewlet/internal/tools/toolschema"
)

// Faults is each reason schema is not a valid input schema for an MCP tool;
// empty for a valid one.
func Faults(schema map[string]any) []string {
	out := toolschema.Faults(schema)
	raw, err := json.Marshal(schema)
	if err != nil {
		return append(out, "does not marshal: "+err.Error())
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return append(out, "does not unmarshal: "+err.Error())
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	const url = "https://crewlet.test/tool.json"
	if err := c.AddResource(url, doc); err != nil {
		return append(out, err.Error())
	}
	if _, err := c.Compile(url); err != nil {
		out = append(out, "fails the draft 2020-12 meta-schema: "+err.Error())
	}
	return out
}
