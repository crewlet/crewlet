package assistanttest

import "testing"

// exampleValues are the values a tool's required arguments are filled with by
// name: each of the shape its name asks for, so a request built from them is
// one the route would parse. A name two tools take in two shapes (a share's
// `to` is one audience, a direct message's is a list of handles) is filled
// only where the value fits the schema, and by its type everywhere else.
var exampleValues = map[string]any{
	"item":         "ENG-42",
	"page":         RecordID,
	"artifact":     RecordID,
	"turn_id":      RecordID,
	"id":           RecordID,
	"handle":       "pm",
	"key":          "ENG",
	"q":            "deploy runbook",
	"to":           "pm",
	"from":         "pm",
	"items":        []any{"ENG-1", "ENG-2"},
	"if_match":     float64(3),
	"base_version": float64(2),
	"action":       "work.filter.apply",
	"screen":       "item",
	"mission":      "Help friends share costs fairly.",
	"vision":       "Settling up takes seconds.",
	"policies":     []any{"Miles is the final authority."},
	"channel":      RecordID,
	"message_id":   RecordID,
	"reply_to":     RecordID,
	"members":      []any{map[string]any{"handle": "pm"}},
	"seat":         "qa",
	"tokens":       float64(1500000),
}

// ExampleArgs is a call's arguments for a tool with the JSON Schema params:
// every argument [exampleValues] names that the schema declares, and every
// other required one as the first of its enum, the least of its range, or a
// short text. For the walks that build every tool's request without a model.
func ExampleArgs(t testing.TB, params map[string]any) map[string]any {
	t.Helper()
	props, _ := params["properties"].(map[string]any)
	out := map[string]any{}
	for name := range props {
		if v, ok := exampleValues[name]; ok && fits(v, props[name]) {
			out[name] = v
		}
	}
	required, _ := params["required"].([]string)
	for _, name := range required {
		if v, ok := exampleValues[name]; ok && fits(v, props[name]) {
			out[name] = v
			continue
		}
		prop, _ := props[name].(map[string]any)
		switch kind, _ := prop["type"].(string); {
		case prop["enum"] != nil:
			out[name] = prop["enum"].([]string)[0]
		case kind == "integer" || kind == "number":
			lo, _ := prop["minimum"].(int)
			out[name] = float64(lo)
		case kind == "boolean":
			out[name] = true
		case kind == "array":
			out[name] = []any{"x"}
		case kind == "string":
			out[name] = "A line of text"
		default:
			t.Fatalf("no example for the required argument %q of type %q", name, kind)
		}
	}
	return out
}

// fits reports whether v is of the type the JSON Schema prop declares.
func fits(v, prop any) bool {
	schema, _ := prop.(map[string]any)
	kind, _ := schema["type"].(string)
	switch v.(type) {
	case string:
		return kind == "string"
	case float64:
		return kind == "integer" || kind == "number"
	case bool:
		return kind == "boolean"
	case []any:
		return kind == "array"
	case map[string]any:
		return kind == "object"
	}
	return false
}
