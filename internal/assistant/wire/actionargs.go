package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// ActionArgs is the shape a page action's arguments take on the screen that
// mounts it NOW, as that screen declares it: a small, closed subset of JSON
// Schema, so the model reads it in the vocabulary it already knows.
//
// DECLARED ONCE, BY THE SCREEN. A page action is the screen's (its parser,
// its gate, its setters), so the screen is the one place that knows which
// keys it reads and which values it takes now: the windows it offers, the
// seats it draws, the spaces it searches. The dashboard builds this schema
// from the same declaration its parser reads, sends it in the page context
// (`action_args`), and the engine holds every proposal to it BEFORE anything
// is previewed or recorded: a proposal the screen could only refuse is the
// model's refused step, never a card in front of the person.
//
// A HINT, NEVER AUTHORITY. The schema only ever refuses more: the screen
// parses again before it previews or applies, and nothing here grants. So the
// engine holds the schema itself to a bounded vocabulary (an unknown keyword,
// a pattern RE2 cannot compile, a schema too deep or too wide is refused as a
// malformed page context), and checks values against it, nothing else.
type ActionArgs struct {
	// Type is the JSON type or types the value takes; empty takes any.
	Type ArgTypes `json:"type,omitempty"`
	// Description says what the value is, in the screen's words.
	Description string `json:"description,omitempty"`
	// Enum and Const close a string to a set of values, or to one.
	Enum  []string `json:"enum,omitempty"`
	Const *string  `json:"const,omitempty"`
	// MinLength and MaxLength bound a string, in UTF-16 code units as
	// JSON Schema counts them; MinItems and MaxItems bound an array.
	MinLength *int `json:"minLength,omitempty"`
	MaxLength *int `json:"maxLength,omitempty"`
	MinItems  *int `json:"minItems,omitempty"`
	MaxItems  *int `json:"maxItems,omitempty"`
	// Items is every member of an array.
	Items *ActionArgs `json:"items,omitempty"`
	// Properties are an object's named members, Required the ones it must
	// carry, and PatternProperties a family of members by the pattern
	// their name matches (`f.<field>`).
	Properties        map[string]*ActionArgs `json:"properties,omitempty"`
	Required          []string               `json:"required,omitempty"`
	PatternProperties map[string]*ActionArgs `json:"patternProperties,omitempty"`
	// AdditionalProperties says whether an object takes a member neither
	// list names. JSON Schema's default is true; every closed object the
	// dashboard declares states false.
	AdditionalProperties *bool `json:"additionalProperties,omitempty"`
	// OneOf is a value that is one of several shapes: an operation of a
	// list of them, told apart by a member every variant pins with Const.
	OneOf []*ActionArgs `json:"oneOf,omitempty"`
	// Examples are whole values the screen takes, shown to the model and
	// held to the schema itself; read at the top level only.
	Examples []json.RawMessage `json:"examples,omitempty"`
}

// ArgType is one JSON type a value may take.
type ArgType string

// The JSON types an [ActionArgs] names.
const (
	ArgObject  ArgType = "object"
	ArgArray   ArgType = "array"
	ArgString  ArgType = "string"
	ArgNumber  ArgType = "number"
	ArgInteger ArgType = "integer"
	ArgBoolean ArgType = "boolean"
	ArgNull    ArgType = "null"
)

// ArgTypeNames are every [ArgType], for a refusal that names them.
var ArgTypeNames = []ArgType{ArgObject, ArgArray, ArgString, ArgNumber, ArgInteger, ArgBoolean,
	ArgNull}

// Valid reports whether t is one of [ArgTypeNames].
func (t ArgType) Valid() bool { return slices.Contains(ArgTypeNames, t) }

// ArgTypes is a schema's `type`: one name, or a list of them, as JSON Schema
// spells either.
type ArgTypes []ArgType

// MarshalJSON writes one type as its name and several as a list.
func (t ArgTypes) MarshalJSON() ([]byte, error) {
	if len(t) == 1 {
		return json.Marshal(string(t[0]))
	}
	return json.Marshal([]ArgType(t))
}

// UnmarshalJSON reads a name or a list of them.
func (t *ArgTypes) UnmarshalJSON(raw []byte) error {
	var one ArgType
	if err := json.Unmarshal(raw, &one); err == nil {
		*t = ArgTypes{one}
		return nil
	}
	var many []ArgType
	if err := json.Unmarshal(raw, &many); err != nil {
		return fmt.Errorf("a type is a name or a list of names")
	}
	*t = many
	return nil
}

// The bounds a declared schema is held to. Generous for every action a screen
// declares (the org builder's operations, the widest, use a depth of five and
// about sixty nodes) and small enough that checking a value against one is
// cheap and the schema reads in a few lines of the prompt.
const (
	actionArgsMaxDepth    = 8
	actionArgsMaxNodes    = 160
	actionArgsMaxEnum     = 64
	actionArgsMaxMembers  = 32
	actionArgsMaxVariants = 16
	actionArgsMaxExamples = 4
	actionArgsMaxPattern  = 128
	actionArgsMaxDescribe = 300
	// ActionArgsMaxBytes is the most one action's schema encodes to.
	ActionArgsMaxBytes = 6 << 10
)

// DecodeActionArgs reads one schema strictly: a keyword this vocabulary does
// not have is refused rather than ignored, because a schema the engine reads
// differently from the screen would refuse what the screen takes.
func DecodeActionArgs(raw []byte) (*ActionArgs, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var out ActionArgs
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: an action's arguments are not a schema this engine reads: "+
			"%w", ErrInvalid, err)
	}
	return &out, out.Validate()
}

// UnmarshalJSON decodes strictly ([DecodeActionArgs]), at every depth.
func (s *ActionArgs) UnmarshalJSON(raw []byte) error {
	type plainArgs ActionArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var out plainArgs
	if err := dec.Decode(&out); err != nil {
		return err
	}
	*s = ActionArgs(out)
	return nil
}

// Validate holds a declared schema to the vocabulary's bounds, and its
// examples to the schema itself: an example the schema refuses is a
// declaration that contradicts itself, and the model would copy it.
func (s *ActionArgs) Validate() error {
	raw, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("%w: an action's arguments do not encode: %w", ErrInvalid, err)
	}
	if len(raw) > ActionArgsMaxBytes {
		return fmt.Errorf("%w: an action's arguments are %d bytes, over %d", ErrInvalid,
			len(raw), ActionArgsMaxBytes)
	}
	nodes := 0
	if err := s.validate("args", 0, &nodes); err != nil {
		return err
	}
	if len(s.Examples) > actionArgsMaxExamples {
		return fmt.Errorf("%w: an action declares %d examples, over %d", ErrInvalid,
			len(s.Examples), actionArgsMaxExamples)
	}
	for i, example := range s.Examples {
		var v any
		if err := json.Unmarshal(example, &v); err != nil {
			return fmt.Errorf("%w: example %d is not JSON", ErrInvalid, i+1)
		}
		if err := s.Check(v); err != nil {
			return fmt.Errorf("%w: example %d does not take its own schema: %w", ErrInvalid,
				i+1, err)
		}
	}
	return nil
}

func (s *ActionArgs) validate(path string, depth int, nodes *int) error {
	*nodes++
	switch {
	case s == nil:
		return fmt.Errorf("%w: %s is no schema", ErrInvalid, path)
	case depth > actionArgsMaxDepth:
		return fmt.Errorf("%w: %s nests deeper than %d", ErrInvalid, path, actionArgsMaxDepth)
	case *nodes > actionArgsMaxNodes:
		return fmt.Errorf("%w: an action's arguments have more than %d parts", ErrInvalid,
			actionArgsMaxNodes)
	case len(s.Enum) > actionArgsMaxEnum:
		return fmt.Errorf("%w: %s lists %d values, over %d", ErrInvalid, path, len(s.Enum),
			actionArgsMaxEnum)
	case len(s.Properties)+len(s.PatternProperties) > actionArgsMaxMembers:
		return fmt.Errorf("%w: %s declares more than %d members", ErrInvalid, path,
			actionArgsMaxMembers)
	case len(s.OneOf) > actionArgsMaxVariants:
		return fmt.Errorf("%w: %s declares more than %d shapes", ErrInvalid, path,
			actionArgsMaxVariants)
	case depth > 0 && len(s.Examples) > 0:
		return fmt.Errorf("%w: %s declares examples below the top", ErrInvalid, path)
	case !text(s.Description, actionArgsMaxDescribe):
		return fmt.Errorf("%w: %s description is not text of at most %d characters",
			ErrInvalid, path, actionArgsMaxDescribe)
	}
	for _, t := range s.Type {
		if !t.Valid() {
			return fmt.Errorf("%w: %s type %q is not one of %v", ErrInvalid, path, t, ArgTypeNames)
		}
	}
	for _, v := range s.Enum {
		if !plain(v, pageValueMaxBytes) {
			return fmt.Errorf("%w: %s lists a value that is not plain text", ErrInvalid, path)
		}
	}
	if s.Const != nil && !plain(*s.Const, pageValueMaxBytes) {
		return fmt.Errorf("%w: %s const is not plain text", ErrInvalid, path)
	}
	for _, bound := range []*int{s.MinLength, s.MaxLength, s.MinItems, s.MaxItems} {
		if bound != nil && *bound < 0 {
			return fmt.Errorf("%w: %s states a negative bound", ErrInvalid, path)
		}
	}
	for _, name := range s.Required {
		if _, ok := s.Properties[name]; !ok {
			return fmt.Errorf("%w: %s requires %q, which it does not declare", ErrInvalid, path,
				name)
		}
	}
	if s.Items != nil {
		if err := s.Items.validate(path+"[]", depth+1, nodes); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
		if !plain(name, actionIDMaxBytes) || name == "" {
			return fmt.Errorf("%w: %s names a member %q that is not a name", ErrInvalid, path,
				name)
		}
		if err := s.Properties[name].validate(path+"."+name, depth+1, nodes); err != nil {
			return err
		}
	}
	for _, pattern := range slices.Sorted(maps.Keys(s.PatternProperties)) {
		if len(pattern) > actionArgsMaxPattern {
			return fmt.Errorf("%w: %s names a member pattern over %d bytes", ErrInvalid, path,
				actionArgsMaxPattern)
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("%w: %s member pattern %q does not compile", ErrInvalid, path,
				pattern)
		}
		if err := s.PatternProperties[pattern].validate(path+"."+pattern, depth+1,
			nodes); err != nil {
			return err
		}
	}
	for i, variant := range s.OneOf {
		if err := variant.validate(fmt.Sprintf("%s(%d)", path, i+1), depth+1,
			nodes); err != nil {
			return err
		}
	}
	return nil
}

// Check reports what is wrong with a value as the schema reads it, in a
// sentence that names the member and what it takes, so the model corrects
// the call rather than guessing again. EVERY FAULT at once, joined in path
// order (ADR-0059): a refusal naming one of several spends the model's next
// attempt on the one after it.
func (s *ActionArgs) Check(v any) error {
	found := s.problems("args", v)
	if len(found) == 0 {
		return nil
	}
	reasons := make([]string, len(found))
	for i, err := range found {
		reasons[i] = err.Error()
	}
	return errors.New(strings.Join(reasons, "; "))
}

func (s *ActionArgs) problems(path string, v any) []error {
	if len(s.Type) > 0 && !slices.ContainsFunc(s.Type, func(t ArgType) bool { return is(t, v) }) {
		return []error{fmt.Errorf("%s is %s", path, typeWords(s.Type))}
	}
	if len(s.OneOf) > 0 {
		return s.oneOfProblems(path, v)
	}
	var found []error
	add := func(format string, args ...any) { found = append(found, fmt.Errorf(format, args...)) }
	switch value := v.(type) {
	case string:
		n := utf16Len(value)
		if s.Const != nil && value != *s.Const {
			add("%s is %q", path, *s.Const)
		}
		if len(s.Enum) > 0 && !slices.Contains(s.Enum, value) {
			add("%s is one of %s, not %q", path, quoted(s.Enum), value)
		}
		if s.MinLength != nil && n < *s.MinLength {
			add("%s is at least %d characters", path, *s.MinLength)
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			add("%s is at most %d characters", path, *s.MaxLength)
		}
	case []any:
		if s.MinItems != nil && len(value) < *s.MinItems {
			add("%s holds at least %d entries", path, *s.MinItems)
		}
		if s.MaxItems != nil && len(value) > *s.MaxItems {
			add("%s holds at most %d entries", path, *s.MaxItems)
		}
		if s.Items != nil {
			for i, item := range value {
				found = append(found, s.Items.problems(fmt.Sprintf("%s[%d]", path, i), item)...)
			}
		}
	case map[string]any:
		found = append(found, s.objectProblems(path, value)...)
	}
	return found
}

func (s *ActionArgs) objectProblems(path string, value map[string]any) []error {
	var found []error
	closed := s.AdditionalProperties != nil && !*s.AdditionalProperties
	for _, name := range slices.Sorted(maps.Keys(value)) {
		member, declared := s.Properties[name]
		if !declared {
			for _, pattern := range slices.Sorted(maps.Keys(s.PatternProperties)) {
				if re, err := regexp.Compile(pattern); err == nil && re.MatchString(name) {
					member, declared = s.PatternProperties[pattern], true
					break
				}
			}
		}
		switch {
		case declared:
			found = append(found, member.problems(path+"."+name, value[name])...)
		case closed:
			found = append(found, fmt.Errorf("%s takes no %q; it takes %s", path, name, s.memberWords()))
		}
	}
	for _, name := range s.Required {
		if _, ok := value[name]; !ok {
			found = append(found, fmt.Errorf("%s needs %q", path, name))
		}
	}
	return found
}

// oneOfProblems holds v to the variant it names: by the member every variant
// pins with a Const (an operation's `op`), so a refusal speaks of the one
// shape the model meant; else to the first variant that takes it, answering
// the first variant's problems when none does.
func (s *ActionArgs) oneOfProblems(path string, v any) []error {
	if key := s.discriminator(); key != "" {
		var values []string
		for _, variant := range s.OneOf {
			pinned := *variant.Properties[key].Const
			values = append(values, pinned)
			if obj, ok := v.(map[string]any); ok && obj[key] == pinned {
				return variant.problems(path, v)
			}
		}
		return []error{fmt.Errorf("%s.%s is one of %s", path, key, quoted(values))}
	}
	var first []error
	for _, variant := range s.OneOf {
		found := variant.problems(path, v)
		if len(found) == 0 {
			return nil
		}
		if first == nil {
			first = found
		}
	}
	return first
}

// discriminator is the member every variant pins with a Const, or "".
func (s *ActionArgs) discriminator() string {
	if len(s.OneOf) == 0 {
		return ""
	}
	for name, p := range s.OneOf[0].Properties {
		if p.Const == nil {
			continue
		}
		if !slices.ContainsFunc(s.OneOf, func(v *ActionArgs) bool {
			m, ok := v.Properties[name]
			return !ok || m.Const == nil
		}) {
			return name
		}
	}
	return ""
}

// memberWords names what an object takes: its members, and its families.
func (s *ActionArgs) memberWords() string {
	names := slices.Sorted(maps.Keys(s.Properties))
	for _, pattern := range slices.Sorted(maps.Keys(s.PatternProperties)) {
		names = append(names, "members matching "+pattern)
	}
	if len(names) == 0 {
		return "nothing"
	}
	return strings.Join(names, ", ")
}

// is reports whether v is a JSON value of type t, as encoding/json decodes one.
func is(t ArgType, v any) bool {
	switch t {
	case ArgObject:
		_, ok := v.(map[string]any)
		return ok
	case ArgArray:
		_, ok := v.([]any)
		return ok
	case ArgString:
		_, ok := v.(string)
		return ok
	case ArgNumber:
		_, ok := numberOf(v)
		return ok
	case ArgInteger:
		n, ok := numberOf(v)
		return ok && n == math.Trunc(n)
	case ArgBoolean:
		_, ok := v.(bool)
		return ok
	case ArgNull:
		return v == nil
	}
	return false
}

// numberOf is a JSON number however it was decoded.
func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// typeWords names the types a value takes, as a sentence ends with them.
func typeWords(types ArgTypes) string {
	words := map[ArgType]string{ArgObject: "an object", ArgArray: "a list",
		ArgString: "text", ArgNumber: "a number", ArgInteger: "a whole number",
		ArgBoolean: "true or false", ArgNull: "null"}
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, words[t])
	}
	return strings.Join(out, " or ")
}

// quoted lists values the way a refusal names them.
func quoted(values []string) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(out, ", ")
}

// utf16Len is a string's length as JSON Schema (and the dashboard) count it.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	if !utf8.ValidString(s) {
		return len(s)
	}
	return n
}
