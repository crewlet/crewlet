package assistanttest

// exampleValues are the values a tool's required arguments are filled with by
// name: each of the shape its name asks for, so a request built from them is
// one the route would parse.
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
}

// ExampleArgs is a call's arguments for a tool with the JSON Schema params:
// every argument [exampleValues] names that the schema declares, and every
// other required one as the first of its enum, the least of its range, or a
// short text. For the walks that build every tool's request without a model.
func ExampleArgs(t TB, params map[string]any) map[string]any {
	t.Helper()
	props, _ := params["properties"].(map[string]any)
	out := map[string]any{}
	for name := range props {
		if v, ok := exampleValues[name]; ok {
			out[name] = v
		}
	}
	required, _ := params["required"].([]string)
	for _, name := range required {
		if v, ok := exampleValues[name]; ok {
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
