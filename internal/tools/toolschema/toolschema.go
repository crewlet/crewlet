// Package toolschema builds a tool's input schema and finds what makes one
// invalid, as the MCP tools/list answer carries it.
//
// ONE INVALID SCHEMA COSTS THE WHOLE SERVER: Claude Code rejects a tools/list
// answer holding a tool whose inputSchema is not a valid JSON Schema (a
// `"required": null`, from a nil []string, was enough) and lists none of the
// server's tools. So every first-party builder makes its object schemas with
// [Object], a sweep holds every first-party tool to [Faults], and the tool
// session leaves out, by name, any tool [Faults] refuses rather than send it.
//
// A leaf over the standard library, so every catalogue and transport can
// import it.
package toolschema

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// Object is an object schema of props that takes no other member, requiring
// each of required. An empty required is left out: JSON Schema has no null
// form of it.
func Object(props map[string]any, required ...string) map[string]any {
	out := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// valueKeywords hold an instance value, and listKeywords a list of them,
// where a null is a value like any other.
var (
	valueKeywords = []string{"default", "const"}
	listKeywords  = []string{"enum", "examples"}
)

// schemaMaps map names to subschemas.
var schemaMaps = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}

// schemaOne hold one subschema; items may also hold a list of them (draft 7).
var schemaOne = []string{"items", "additionalProperties", "not", "if", "then", "else", "contains",
	"propertyNames", "unevaluatedProperties", "unevaluatedItems", "additionalItems"}

// schemaLists hold a list of subschemas.
var schemaLists = []string{"allOf", "anyOf", "oneOf", "prefixItems"}

// Faults names each reason schema is not a valid input schema for an MCP
// tool, as it marshals: an object schema of `type: "object"`, no keyword
// null, each `required` a list of distinct strings, every one a property
// where the schema admits no other. Empty for a valid one.
func Faults(schema map[string]any) []string {
	raw, err := json.Marshal(schema)
	if err != nil {
		return []string{"does not marshal: " + err.Error()}
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return []string{"does not unmarshal: " + err.Error()}
	}
	m, ok := root.(map[string]any)
	if !ok {
		return []string{"is not an object"}
	}
	var out []string
	if m["type"] != "object" {
		out = append(out, fmt.Sprintf("type is %v, not \"object\"", m["type"]))
	}
	w := walker{out: out}
	w.schema(m, "")
	return w.out
}

type walker struct{ out []string }

func (w *walker) add(path, format string, args ...any) {
	if path == "" {
		path = "the schema"
	}
	w.out = append(w.out, path+" "+fmt.Sprintf(format, args...))
}

func at(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// schema walks one schema: a boolean, or an object of keywords.
func (w *walker) schema(v any, path string) {
	if _, ok := v.(bool); ok {
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		w.add(path, "is %s, not a schema", kind(v))
		return
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		val, p := m[k], at(path, k)
		switch {
		case slices.Contains(valueKeywords, k):
		case slices.Contains(listKeywords, k):
			if _, ok := val.([]any); !ok {
				w.add(p, "is %s, not a list", kind(val))
			}
		case val == nil:
			w.add(p, "is null")
		case k == "required":
			w.required(m, val, p)
		case k == "type":
			w.typ(val, p)
		case slices.Contains(schemaMaps, k):
			sub, ok := val.(map[string]any)
			if !ok {
				w.add(p, "is %s, not an object", kind(val))
				continue
			}
			for _, name := range slices.Sorted(maps.Keys(sub)) {
				w.schema(sub[name], at(p, name))
			}
		case slices.Contains(schemaOne, k):
			if list, ok := val.([]any); ok && k == "items" {
				w.list(list, p)
				continue
			}
			w.schema(val, p)
		case slices.Contains(schemaLists, k):
			list, ok := val.([]any)
			if !ok {
				w.add(p, "is %s, not a list", kind(val))
				continue
			}
			w.list(list, p)
		default:
			w.noNull(val, p)
		}
	}
}

func (w *walker) list(list []any, path string) {
	for i, s := range list {
		w.schema(s, fmt.Sprintf("%s[%d]", path, i))
	}
}

// required holds a list of distinct strings, each a property where the
// schema admits no other member, so none asks for what no call can send.
func (w *walker) required(schema map[string]any, v any, path string) {
	list, ok := v.([]any)
	if !ok {
		w.add(path, "is %s, not a list", kind(v))
		return
	}
	props, _ := schema["properties"].(map[string]any)
	closed := schema["additionalProperties"] == false && schema["patternProperties"] == nil
	seen := map[string]bool{}
	for i, e := range list {
		name, ok := e.(string)
		switch {
		case !ok:
			w.add(fmt.Sprintf("%s[%d]", path, i), "is %s, not a string", kind(e))
		case seen[name]:
			w.add(path, "names %q twice", name)
		case closed && props[name] == nil:
			w.add(path, "names %q, which is not a property", name)
		}
		seen[name] = true
	}
}

// typ holds a type name or a list of them.
func (w *walker) typ(v any, path string) {
	switch t := v.(type) {
	case string:
	case []any:
		for i, e := range t {
			if _, ok := e.(string); !ok {
				w.add(fmt.Sprintf("%s[%d]", path, i), "is %s, not a type name", kind(e))
			}
		}
	default:
		w.add(path, "is %s, not a type name", kind(v))
	}
}

// noNull holds a keyword's value to no null at any depth.
func (w *walker) noNull(v any, path string) {
	switch t := v.(type) {
	case nil:
		w.add(path, "is null")
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			w.noNull(t[k], at(path, k))
		}
	case []any:
		for i, e := range t {
			w.noNull(e, fmt.Sprintf("%s[%d]", path, i))
		}
	}
}

func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "a string"
	case float64:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}
