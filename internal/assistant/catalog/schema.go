package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

// IdentityArguments are the argument names no assistant tool may declare,
// because each names who acts, where, or which operation a write is: the
// company, a principal, an actor, a login, a credential or an operation id.
// Identity is the grant the call is sent under and nothing else (ADR-0050).
// An argument naming a person or a seat as the SUBJECT of a read (`handle`) is
// not one of these: the authority table decides that read.
var IdentityArguments = []string{
	"company", "company_id", "principal", "person_id", "actor", "as", "login",
	"op_id", "operator", "operator_id", "via", "on_behalf_of", "idempotency_key",
}

// ErrInvalidArguments is a call whose arguments the tool's schema refuses: an
// argument it does not declare, an identity argument, a missing required one,
// or a value of the wrong kind or out of its range. Nothing is sent.
var ErrInvalidArguments = errors.New("catalog: the arguments are refused")

// schema is the subset of JSON Schema a catalogue tool's arguments are written
// in, checked here before anything is built so a tool never sends a request
// its own schema would refuse.
type schema struct {
	kind        string // string, integer, number, boolean, array, object
	description string
	enum        []string
	// maxLen bounds a string in bytes, or in characters where chars is set,
	// maxItems an array; min and max an integer. Zero is unbounded except
	// where hasMin is set.
	maxLen, maxItems int
	// chars counts maxLen in characters (Unicode code points), the unit JSON
	// Schema's maxLength is defined in, so the bound the model is handed is
	// the bound it is held to ([chars]).
	chars          bool
	min, max       int
	hasMin, hasMax bool
	items          *schema
	properties     map[string]*schema
	required       []string
	// freeform is an object whose members its reader declares (a page
	// action's payload): any member is taken, an identity name excepted.
	freeform bool
	// payload is a request's own parameters or body, which a generic API
	// call carries to the route that reads them ([APIRequestTool]): any JSON
	// value is taken, nested as deep as the route reads it, and NO NAME IS
	// REFUSED. An identity argument names who acts, and a generic call
	// names no actor: identity is the grant it is sent under, which no
	// route reads from a parameter or a body, and a body's `login` is the
	// login of the person a directory edit is about, which the route then
	// decides as the person's own gesture.
	payload bool
}

// JSON is the schema as the model is handed it.
func (s *schema) JSON() map[string]any {
	out := map[string]any{"type": s.kind}
	if s.description != "" {
		out["description"] = s.description
	}
	if len(s.enum) > 0 {
		out["enum"] = slices.Clone(s.enum)
	}
	if s.maxLen > 0 {
		out["maxLength"] = s.maxLen
	}
	if s.maxItems > 0 {
		out["maxItems"] = s.maxItems
	}
	if s.hasMin {
		out["minimum"] = s.min
	}
	if s.hasMax {
		out["maximum"] = s.max
	}
	if s.items != nil {
		out["items"] = s.items.JSON()
	}
	if s.kind == "object" && !s.freeform && !s.payload {
		props := map[string]any{}
		for name, p := range s.properties {
			props[name] = p.JSON()
		}
		out["properties"] = props
		out["additionalProperties"] = false
		if len(s.required) > 0 {
			out["required"] = slices.Clone(s.required)
		}
	}
	return out
}

// problems is EVERYTHING wrong with v as this schema reads it, in a stable
// order (an object's members by name, then the ones it lacks), so a caller
// that lets its model write a call again can name every fault at once rather
// than one per attempt.
func (s *schema) problems(path string, v any) []error {
	switch s.kind {
	case "string":
		str, ok := v.(string)
		if !ok {
			return []error{fmt.Errorf("%s is a string", path)}
		}
		if err := s.checkString(path, str); err != nil {
			return []error{err}
		}
	case "integer", "number":
		n, ok := number(v)
		if !ok || (s.kind == "integer" && n != math.Trunc(n)) {
			return []error{fmt.Errorf("%s is %s", path, map[string]string{
				"integer": "a whole number", "number": "a number"}[s.kind])}
		}
		if (s.hasMin && n < float64(s.min)) || (s.hasMax && n > float64(s.max)) {
			return []error{fmt.Errorf("%s is from %d to %d", path, s.min, s.max)}
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return []error{fmt.Errorf("%s is true or false", path)}
		}
	case "array":
		list, ok := v.([]any)
		if !ok {
			return []error{fmt.Errorf("%s is a list", path)}
		}
		var found []error
		if s.maxItems > 0 && len(list) > s.maxItems {
			found = append(found, fmt.Errorf("%s holds at most %d entries", path, s.maxItems))
		}
		for i, e := range list {
			found = append(found, s.items.problems(fmt.Sprintf("%s[%d]", path, i), e)...)
		}
		return found
	case "object":
		return s.objectProblems(path, v)
	default:
		return []error{fmt.Errorf("%s has no type this catalogue checks", path)}
	}
	return nil
}

// checkString holds a string to its bound, its encoding and its values. The
// bound is in characters where the schema says so ([schema.chars]), in bytes
// otherwise.
func (s *schema) checkString(path, str string) error {
	switch {
	case s.maxLen > 0 && s.chars && utf8.RuneCountInString(str) > s.maxLen:
		return fmt.Errorf("%s is %d characters and the most it takes is %d", path,
			utf8.RuneCountInString(str), s.maxLen)
	case s.maxLen > 0 && !s.chars && len(str) > s.maxLen:
		return fmt.Errorf("%s is %d bytes and the most it takes is %d", path, len(str),
			s.maxLen)
	case !utf8.ValidString(str):
		return fmt.Errorf("%s is not valid UTF-8", path)
	case len(s.enum) > 0 && !slices.Contains(s.enum, str) &&
		utf8.RuneCountInString(str) <= enumEcho:
		return fmt.Errorf("%s is one of %s, not %q", path, strings.Join(s.enum, ", "), str)
	case len(s.enum) > 0 && !slices.Contains(s.enum, str):
		return fmt.Errorf("%s is one of %s", path, strings.Join(s.enum, ", "))
	}
	return nil
}

// objectProblems is everything wrong with an object: an identity name, a
// member it does not declare or one out of its schema, and a required member
// it lacks.
func (s *schema) objectProblems(path string, v any) []error {
	obj, ok := v.(map[string]any)
	if !ok {
		return []error{fmt.Errorf("%s is an object", path)}
	}
	if s.payload {
		return nil
	}
	var found []error
	for _, name := range slices.Sorted(maps.Keys(obj)) {
		if slices.Contains(IdentityArguments, name) {
			found = append(found, fmt.Errorf("%s names %q, and identity is the person's own "+
				"and never an argument", path, name))
			continue
		}
		if s.freeform {
			continue
		}
		p, declared := s.properties[name]
		if !declared {
			found = append(found, fmt.Errorf("%s takes no %q; it takes %s", path, name,
				strings.Join(slices.Sorted(maps.Keys(s.properties)), ", ")))
			continue
		}
		found = append(found, p.problems(path+"."+name, obj[name])...)
	}
	for _, name := range s.required {
		if _, ok := obj[name]; !ok {
			found = append(found, fmt.Errorf("%s needs %q", path, name))
		}
	}
	return found
}

// number is a JSON number however it was decoded.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// The builders the catalogue's schemas are written with.

func object(required []string, props map[string]*schema) *schema {
	return &schema{kind: "object", properties: props, required: required}
}

func text(description string, maxLen int) *schema {
	return &schema{kind: "string", description: description, maxLen: maxLen}
}

// chars is a string of at most max characters, as a reader sees its length:
// the bound the model is handed as maxLength is exactly the one it is held
// to. A bytes bound ([text]) is a ceiling the model cannot count against.
func chars(description string, max int) *schema {
	return &schema{kind: "string", description: description, maxLen: max, chars: true}
}

func oneOf(description string, values ...string) *schema {
	return &schema{kind: "string", description: description, enum: values}
}

// positive is a whole number from 1 to hi. NEVER BELOW ONE: every number a
// tool takes is a count, a window or a version, and none of them means
// anything at zero.
func positive(description string, hi int) *schema {
	return &schema{kind: "integer", description: description, min: 1, max: hi,
		hasMin: true, hasMax: true}
}

func flag(description string) *schema {
	return &schema{kind: "boolean", description: description}
}

// names is a list of at most maxItems identifiers (keys, ids, labels), each
// held to [nameBytes]: the only lists a tool takes.
func names(description string, maxItems int) *schema {
	return &schema{kind: "array", description: description, maxItems: maxItems,
		items: &schema{kind: "string", maxLen: nameBytes}}
}

// The bounds every catalogue string is held to, in bytes.
const (
	// enumEcho is the longest value a refusal of a value outside its enum
	// quotes back: a misspelt name is worth showing, and a line of prose
	// written where a name belongs is not repeated into every log and
	// step that carries the refusal.
	enumEcho = 32
	// nameBytes bounds an identifier: a task key, a handle, an id.
	nameBytes = 128
	// queryBytes is knowledge.MaxQueryBytes, the one bound on a ranked
	// search's text: a search on part of a question answers another one.
	queryBytes = 400
	// lineBytes bounds a title or a one-line reason.
	lineBytes = 500
	// bodyBytes bounds a comment, a note or a page body a write proposes:
	// the act route's own body ceiling is twice the largest page plus
	// headroom, so a proposal never carries what the route would refuse.
	bodyBytes = 256 << 10
)

// Args reads one argument as the type its schema declares, after [Tool.Check].
type Args map[string]any

// String is a string argument, "" when absent.
func (a Args) String(name string) string {
	s, _ := a[name].(string)
	return strings.TrimSpace(s)
}

// Int is a whole-number argument, and false when absent.
func (a Args) Int(name string) (int, bool) {
	n, ok := number(a[name])
	return int(n), ok
}

// Bool is a boolean argument, false when absent.
func (a Args) Bool(name string) bool {
	b, _ := a[name].(bool)
	return b
}

// Strings is a list-of-strings argument.
func (a Args) Strings(name string) []string {
	list, _ := a[name].([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}
