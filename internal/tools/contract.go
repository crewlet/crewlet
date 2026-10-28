package tools

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// ---- a call held to the schema its tool advertises (ADR-0059) ------------ //
//
// EVERY FIRST-PARTY CALL IS CHECKED against the JSON schema its tool hands the
// model, in one pass, and each finding is one of three kinds, decided by what
// the tool's own reading of the value would do ([argval], the one rule the
// tools and this check share):
//
//   - A MISREAD is a value the tool would change without saying so: a string
//     where a boolean belongs (read as false), an object or a list where a
//     string belongs (read as ""), a fraction where a whole number belongs
//     (truncated), "5 days" where a number belongs (read as 5), junk the read
//     replaces with its default. It writes wrong data, and a note after the
//     write is too late, so the call is refused before the tool runs.
//   - OUTSIDE is a value the schema refuses and the tool decides on itself: a
//     missing required argument, a value not in an enum, a number past its
//     bound, a list too long. The tool runs; its refusal, or its answer,
//     carries the finding.
//   - UNKNOWN is a key the schema does not declare, which every tool ignores
//     today: noted on the answer, logged and counted, never refused here.
//
// THE LOSSLESS LENIENCIES STAY ACCEPTED, exactly as the tools read them: a
// number or a boolean where a string belongs, one string or a comma-separated
// one where a list of strings belongs, a numeric string where a number
// belongs. They are not findings at all.

// Finding is what an [ArgFault] is: a named string, so a value off the wire
// is one a reader can branch on and [Finding.Valid] can judge.
type Finding string

// The findings, in the order a call's answer lists them ([Findings]).
const (
	// Misread: the tool would read the value as something else.
	Misread Finding = "misread"
	// Outside: the schema refuses it, and the tool decides.
	Outside Finding = "outside"
	// Unknown: a key the schema does not declare.
	Unknown Finding = "unknown"
)

// Findings are every finding this build names, in the order an answer lists
// them.
var Findings = []Finding{Misread, Outside, Unknown}

// Valid reports whether f is a finding this build names.
func (f Finding) Valid() bool { return slices.Contains(Findings, f) }

// ArgFault is one way a call's arguments are outside its tool's schema: the
// argument's path (`limit`, `ops[2].if_version`), the kind of finding, and a
// sentence that names the path and the shape the tool reads there.
type ArgFault struct {
	Path    string
	Finding Finding
	Message string
}

// CheckArgs holds a call's arguments to its tool's advertised schema, naming
// every finding in one pass, in path order. A schema keyword this check does
// not implement is held to nothing here ([UnsupportedSchema] names them, and
// a test holds every first-party schema to none).
func CheckArgs(tool string, schema, args map[string]any) []ArgFault {
	c := &contract{tool: tool}
	var root any = args
	if args == nil {
		root = map[string]any{}
	}
	c.value(schema, "", root)
	return c.found
}

type contract struct {
	tool  string
	found []ArgFault
}

func (c *contract) add(path string, f Finding, format string, args ...any) {
	c.found = append(c.found, ArgFault{Path: path, Finding: f, Message: fmt.Sprintf(format, args...)})
}

// value holds one value to its schema.
func (c *contract) value(schema map[string]any, path string, v any) {
	if schema == nil {
		return
	}
	if v == nil {
		// NULL IS ABSENT to every tool's read, so it is checked as a
		// missing value by the object that holds it, never as a type.
		return
	}
	types := toolargs.TypesOf(schema)
	if len(types) > 0 && !c.fits(schema, types, path, v) {
		return
	}
	switch v := v.(type) {
	case map[string]any:
		c.object(schema, path, v)
	case []any:
		c.list(schema, path, v)
	case string:
		if slices.Contains(types, "array") && !slices.Contains(types, "string") {
			// ONE STRING STANDING FOR A LIST is checked as the list the
			// tool reads it as, element by element.
			items, _ := argval.Strings(v)
			list := make([]any, len(items))
			for i, item := range items {
				list[i] = item
			}
			c.list(schema, path, list)
			return
		}
		c.enum(schema, path, v)
		c.bounds(schema, path, v)
	default:
		if s, ok := argval.String(v); ok {
			c.enum(schema, path, s)
		}
		c.bounds(schema, path, v)
	}
}

// fits reports whether v is a value of one of types as the tool reads it,
// reporting the misread when it is not.
func (c *contract) fits(schema map[string]any, types []string, path string, v any) bool {
	for _, t := range types {
		if readsAs(t, schema, v) {
			return true
		}
	}
	c.add(path, Misread, "%s is %s, and %s reads %s there", c.name(path), what(v), c.tool,
		describe(schema))
	return false
}

// readsAs reports whether the tool's read of a value of type t takes v as the
// author meant it ([argval]).
func readsAs(t string, schema map[string]any, v any) bool {
	switch t {
	case "string":
		_, ok := argval.String(v)
		return ok
	case "integer":
		_, ok := argval.Whole(v)
		return ok
	case "number":
		_, ok := argval.Number(v)
		return ok
	case "boolean":
		_, ok := argval.Bool(v)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		switch v.(type) {
		case []any:
			return true
		case string:
			// ONE STRING, or a comma-separated one, is a list of
			// strings to the tool's read, and nothing else is.
			items, _ := schema["items"].(map[string]any)
			return items == nil || slices.Contains(toolargs.TypesOf(items), "string")
		}
		return false
	case "null":
		return v == nil
	}
	return true
}

// object holds an object's members: each declared one to its schema, each
// required one present, and each undeclared one noted.
func (c *contract) object(schema map[string]any, path string, v map[string]any) {
	props, declared := schema["properties"].(map[string]any)
	extra, extraSchema := schema["additionalProperties"].(map[string]any)
	for _, key := range slices.Sorted(maps.Keys(v)) {
		p := member(path, key)
		switch sub, ok := props[key].(map[string]any); {
		case ok:
			c.value(sub, p, v[key])
		case extraSchema:
			c.value(extra, p, v[key])
		case declared && schema["additionalProperties"] != true:
			c.add(p, Unknown, "%s is not an argument %s takes there, so it was not read: %s takes %s",
				c.name(p), c.tool, c.where(path), strings.Join(slices.Sorted(maps.Keys(props)), ", "))
		}
	}
	for _, key := range toolargs.StringsOf(schema["required"]) {
		if v[key] == nil {
			p := member(path, key)
			c.add(p, Outside, "%s is required: %s", c.name(p), describe(asSchema(props[key])))
		}
	}
}

// list holds a list's length and each element.
func (c *contract) list(schema map[string]any, path string, v []any) {
	if most, ok := intOf(schema["maxItems"]); ok && len(v) > most {
		c.add(path, Outside, "%s holds %d entries, and %s takes at most %d", c.name(path), len(v), c.tool, most)
	}
	if least, ok := intOf(schema["minItems"]); ok && len(v) < least {
		c.add(path, Outside, "%s holds %d entries, and %s takes at least %d", c.name(path), len(v), c.tool, least)
	}
	items, _ := schema["items"].(map[string]any)
	for i, e := range v {
		c.value(items, path+"["+strconv.Itoa(i)+"]", e)
	}
}

// enum holds a string to its schema's closed set.
func (c *contract) enum(schema map[string]any, path, s string) {
	set := toolargs.StringsOf(schema["enum"])
	if len(set) == 0 {
		return
	}
	if _, ok := argval.Choice(s, set); ok {
		return
	}
	c.add(path, Outside, "%s is %q, and %s takes one of %s", c.name(path), clipped(s), c.tool,
		strings.Join(set, ", "))
}

// bounds holds a number to its schema's minimum and maximum.
func (c *contract) bounds(schema map[string]any, path string, v any) {
	n, ok := argval.Number(v)
	if !ok {
		return
	}
	if least, has := numberOf(schema["minimum"]); has && n < least {
		c.add(path, Outside, "%s is %v, and %s takes at least %v", c.name(path), n, c.tool, least)
	}
	if most, has := numberOf(schema["maximum"]); has && n > most {
		c.add(path, Outside, "%s is %v, and %s takes at most %v", c.name(path), n, c.tool, most)
	}
}

// name is a path as a finding names it.
func (c *contract) name(path string) string {
	if path == "" {
		return "the arguments"
	}
	return "`" + path + "`"
}

// where is the object a key list is about.
func (c *contract) where(path string) string {
	if path == "" {
		return c.tool
	}
	return "`" + path + "`"
}

func member(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// describe is a schema's shape as a finding names it: "a whole number", "true
// or false", "a list of strings", "an object {title, status}", "one of
// todo, done".
func describe(schema map[string]any) string {
	if schema == nil {
		return "a value"
	}
	if set := toolargs.StringsOf(schema["enum"]); len(set) > 0 {
		return "one of " + strings.Join(set, ", ")
	}
	var parts []string
	for _, t := range toolargs.TypesOf(schema) {
		switch t {
		case "string":
			parts = append(parts, "a string")
		case "integer":
			parts = append(parts, "a whole number")
		case "number":
			parts = append(parts, "a number")
		case "boolean":
			parts = append(parts, "true or false")
		case "object":
			if props, ok := schema["properties"].(map[string]any); ok && len(props) > 0 {
				parts = append(parts, "an object {"+strings.Join(slices.Sorted(maps.Keys(props)), ", ")+"}")
			} else {
				parts = append(parts, "an object")
			}
		case "array":
			parts = append(parts, "a list of "+plural(asSchema(schema["items"])))
		case "null":
			parts = append(parts, "null")
		}
	}
	if len(parts) == 0 {
		return "a value"
	}
	return strings.Join(parts, " or ")
}

// plural is an element's shape, as a list's is named.
func plural(schema map[string]any) string {
	d := describe(schema)
	switch {
	case d == "a string":
		return "strings (or one string, or a comma-separated one)"
	case d == "a whole number":
		return "whole numbers"
	case d == "a number":
		return "numbers"
	case d == "true or false":
		return "true or false values"
	case strings.HasPrefix(d, "an object"):
		return "objects" + strings.TrimPrefix(d, "an object")
	}
	return "values (" + d + ")"
}

// what is a value as a finding names it: its kind, and a short value.
func what(v any) string {
	switch v := v.(type) {
	case string:
		return fmt.Sprintf("the string %q", clipped(v))
	case bool:
		return strconv.FormatBool(v)
	case map[string]any:
		return "an object"
	case []any:
		return "a list"
	case json.Number:
		return "the number " + v.String()
	case float64:
		return "the number " + strconv.FormatFloat(v, 'g', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}

// clipped is a value short enough to quote in a finding.
func clipped(s string) string {
	const most = 60
	if len(s) <= most {
		return s
	}
	cut := most
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "..."
}

func asSchema(v any) map[string]any {
	s, _ := v.(map[string]any)
	return s
}

func intOf(v any) (int, bool) {
	n, ok := argval.Whole(v)
	return n, ok && v != nil
}

func numberOf(v any) (float64, bool) {
	if v == nil {
		return 0, false
	}
	return argval.Number(v)
}

// schemaKeywords are every keyword [CheckArgs] holds a call to, and the ones it
// reads only to describe a shape. A first-party schema carrying any other is
// one whose promise this check cannot keep, which [UnsupportedSchema] names.
var schemaKeywords = []string{"type", "description", "properties", "required", "items", "enum",
	"maxItems", "minItems", "minimum", "maximum", "additionalProperties"}

// schemaTypes are the `type` values [CheckArgs] reads.
var schemaTypes = []string{"string", "integer", "number", "boolean", "object", "array", "null"}

// UnsupportedSchema names every keyword and type in a tool's schema that
// [CheckArgs] does not implement, so a test holds every first-party schema to
// what the check can keep. Empty for a schema it covers whole.
func UnsupportedSchema(schema map[string]any) []string {
	var out []string
	var walk func(s map[string]any, path string)
	walk = func(s map[string]any, path string) {
		for _, k := range slices.Sorted(maps.Keys(s)) {
			if !slices.Contains(schemaKeywords, k) {
				out = append(out, path+": keyword "+k)
			}
		}
		for _, t := range toolargs.TypesOf(s) {
			if !slices.Contains(schemaTypes, t) {
				out = append(out, path+": type "+t)
			}
		}
		if props, ok := s["properties"].(map[string]any); ok {
			for _, k := range slices.Sorted(maps.Keys(props)) {
				if sub, ok := props[k].(map[string]any); ok {
					walk(sub, member(path, k))
				} else {
					out = append(out, member(path, k)+": not a schema")
				}
			}
		}
		if items, ok := s["items"].(map[string]any); ok {
			walk(items, path+"[]")
		}
		if extra, ok := s["additionalProperties"].(map[string]any); ok {
			walk(extra, path+"{}")
		}
	}
	walk(schema, "")
	return out
}

// ---- the answer ----------------------------------------------------------- //

// maxArgFaults bounds the findings one answer lists; the rest are counted.
const maxArgFaults = 20

var contractLog = logging.Get("tools.contract")

// misread reports whether any finding is one the tool would misread.
func misread(faults []ArgFault) bool {
	return slices.ContainsFunc(faults, func(f ArgFault) bool { return f.Finding == Misread })
}

// misreadAnswer is the refusal of a call holding a value its tool would
// misread: nothing ran, and EVERY finding is listed, the misreads first, so
// one corrected call answers them all.
func misreadAnswer(tool string, faults []ArgFault) string {
	return fmt.Sprintf("%s did not run, so nothing was done: it would have read some of these "+
		"arguments as something other than what you sent. Fix every one and call it again:\n%s",
		tool, faultLines(faults))
}

// faultLines are findings one to a line, misreads first, then by path, at
// most [maxArgFaults] and the rest counted.
func faultLines(faults []ArgFault) string {
	sorted := slices.Clone(faults)
	slices.SortStableFunc(sorted, func(a, b ArgFault) int {
		if c := cmp.Compare(slices.Index(Findings, a.Finding), slices.Index(Findings, b.Finding)); c != 0 {
			return c
		}
		return cmp.Compare(a.Path, b.Path)
	})
	lines := make([]string, 0, min(len(sorted), maxArgFaults)+1)
	for i, f := range sorted {
		if i == maxArgFaults {
			lines = append(lines, fmt.Sprintf("- and %d more", len(sorted)-maxArgFaults))
			break
		}
		lines = append(lines, "- "+f.Message)
	}
	return strings.Join(lines, "\n")
}
