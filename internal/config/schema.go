package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// Tier names one of the two config documents.
type Tier string

// The two tiers a schema can be generated for.
const (
	TierBootstrap Tier = "bootstrap"
	TierCompany   Tier = "company"
)

// Schema returns the JSON Schema for a tier, as indented JSON.
//
// # Who reads this
//
// Not the engine. The engine validates with the Go code in this package;
// the schema is a PUBLIC, EDITOR-FACING artifact — it backs the
// `# yaml-language-server: $schema=` modeline at the top of every shipped
// config, a CI linter, and an assistant authoring a config without the
// binary installed. Its whole value is telling an author about a mistake
// while they are still typing.
//
// # It is a SUBSET of the validator, never a superset
//
// A schema can express structure — key spaces, types, closed sets, ranges,
// patterns, and a handful of cross-field implications. It cannot express
// everything [Company.Validate] checks, and it must never try to: an editor
// that red-underlines a config the engine would happily run teaches authors
// to ignore it.
//
// So the invariant is one-directional and it is tested (see the schema
// parity test): EVERYTHING THE SCHEMA REJECTS, THE VALIDATOR ALSO REJECTS.
// The reverse does not hold, and the validator remains the authority.
//
// Closed sets are read from the same package-level slices the validators
// use, so an enum here cannot drift from what the engine accepts.
func Schema(tier Tier) ([]byte, error) {
	var root reflect.Type
	var title, id string
	var rules []any

	switch tier {
	case TierBootstrap:
		root = reflect.TypeOf(Bootstrap{})
		title = "Crewlet bootstrap config (Tier A)"
		id = "https://docs.crewlet.ai/schema/bootstrap.schema.json"
		rules = bootstrapRules()
	case TierCompany:
		root = reflect.TypeOf(Company{})
		title = "Crewlet company config (Tier B)"
		id = "https://docs.crewlet.ai/schema/company.schema.json"
		rules = companyRules()
	default:
		return nil, fault("", ErrUnknownValue, "unknown schema tier %q (want %s or %s)",
			tier, TierBootstrap, TierCompany)
	}

	g := &schemaGen{defs: map[string]map[string]any{}}
	body := g.structSchema(root)
	doc := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     id,
		"title":   title,
	}
	for k, v := range body {
		doc[k] = v
	}
	if len(rules) > 0 {
		doc["allOf"] = rules
	}
	if len(g.defs) > 0 {
		defs := make(map[string]any, len(g.defs))
		for name, def := range g.defs {
			defs[name] = def
		}
		doc["$defs"] = defs
	}
	return json.MarshalIndent(doc, "", "  ")
}

// schemaGen walks the struct tree once, registering each named struct in
// $defs so a recursive type (a unit holding child units) terminates.
//
// IT CANNOT FAIL, and that is a property of what it now describes rather
// than an omission. Every shape here has a rule: a struct, a scalar, a
// slice, a map, or one of the hand-written [overrides]. It once carried a
// fault channel for the one directive that could meet a shape it had no rule
// for: an integration block the build had no parser for was refused outright.
// That directive is gone with the last vendor it refused — all six route end
// to end — so the channel has nothing left to report. An error return nothing
// can populate reads as a guard against something.
type schemaGen struct {
	defs map[string]map[string]any
}

// overrides are the types the walk cannot see through, because their YAML
// shape is decided by a custom unmarshaler rather than by their fields.
//
// Each is a hand-written fragment. Keeping the list SHORT is the point: a
// fragment is a second description of a decoder, and every entry here is
// something that can silently drift from it. Nothing else in the package
// gets a custom decoder without earning a place in this table.
func overrides() map[reflect.Type]map[string]any {
	str := map[string]any{"type": "string"}
	strList := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	toggle := map[string]any{"type": []any{"boolean", "null"}}

	phases := map[string]any{}
	for _, name := range []string{"default", "plan", "execute", "review", "subagent", "auxiliary", "judge", "sandbox"} {
		phases[name] = map[string]any{"oneOf": []any{str, strList}}
	}

	annotations := map[string]any{}
	for _, name := range []string{"read_only", "destructive", "idempotent", "open_world"} {
		annotations[name] = toggle
	}
	// The protocol's own camelCase spellings decode too, so an editor must
	// accept what the decoder accepts.
	for alias := range annotationAliases {
		annotations[alias] = toggle
	}

	return map[reflect.Type]map[string]any{
		reflect.TypeOf(org.Toggle{}): toggle,
		reflect.TypeOf(org.ProviderKeys(nil)): {
			"oneOf":       []any{str, strList},
			"description": "A provider key, or a fallback chain of them.",
		},
		reflect.TypeOf(PhaseLLM{}): {
			"oneOf": []any{str, strList, map[string]any{
				"type":                 "object",
				"properties":           phases,
				"additionalProperties": false,
			}},
			"description": "A provider key, a fallback chain, or a per-phase mapping.",
		},
		reflect.TypeOf(ToolAnnotations{}): {
			"type":                 "object",
			"properties":           annotations,
			"additionalProperties": false,
		},
	}
}

var schemaOverrides = overrides()

// structSchema builds the object schema for a struct type.
func (g *schemaGen) structSchema(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string

	// Embedded structs contribute their fields to the enclosing object,
	// which is what yaml.v3 does when decoding one.
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for i := range typ.NumField() {
			f := typ.Field(i)
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				walk(f.Type)
				continue
			}
			if f.PkgPath != "" {
				continue // unexported
			}
			name, ok := yamlName(f)
			if !ok {
				continue
			}
			directives := parseDirectives(f.Tag.Get("js"))
			schema := g.fieldSchema(f.Type, directives)
			if desc := f.Tag.Get("desc"); desc != "" {
				schema["description"] = desc
			}
			props[name] = schema
			if _, isRequired := directives["required"]; isRequired {
				required = append(required, name)
			}
		}
	}
	walk(t)

	out := map[string]any{
		"type":       "object",
		"properties": props,
		// An unknown key is an ERROR in the loader, so the schema has to
		// say so too — a schema that quietly allowed extra keys would tell
		// an author their typo is fine right up until the engine refuses
		// to boot on it.
		"additionalProperties": false,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// ref registers a named struct in $defs and returns a reference to it.
func (g *schemaGen) ref(t reflect.Type) map[string]any {
	name := t.Name()
	if name == "" {
		return g.structSchema(t) // an anonymous struct has nothing to name
	}
	if _, done := g.defs[name]; !done {
		// Reserve the name BEFORE recursing: a unit holds child units, so
		// a walk that registered on the way out would never come back.
		g.defs[name] = map[string]any{}
		g.defs[name] = g.structSchema(t)
	}
	return map[string]any{"$ref": "#/$defs/" + name}
}

// fieldSchema maps one Go type onto a schema fragment.
func (g *schemaGen) fieldSchema(t reflect.Type, directives map[string]string) map[string]any {
	if frag, ok := schemaOverrides[t]; ok {
		return cloneSchema(frag)
	}
	switch t.Kind() {
	case reflect.Pointer:
		// An optional block. Absence is expressed by the field not being
		// required, so the pointer itself adds nothing to the schema.
		return g.fieldSchema(t.Elem(), directives)
	case reflect.String:
		out := map[string]any{"type": "string"}
		applyStringDirectives(out, directives)
		return out
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		out := map[string]any{"type": "integer"}
		applyNumberDirectives(out, directives)
		return out
	case reflect.Float32, reflect.Float64:
		out := map[string]any{"type": "number"}
		applyNumberDirectives(out, directives)
		return out
	case reflect.Slice, reflect.Array:
		// THE DIRECTIVES GO TO THE ELEMENTS, not to the list. A closed set
		// on a list field can only mean "each entry is one of these" —
		// there is nothing else a `js:"enum=a|b"` on a `[]T` could say —
		// and dropping them here produced an unconstrained array with no
		// error and no warning: the tag was written, the schema shipped
		// without it, and an editor waved through a value the engine
		// refuses.
		return map[string]any{"type": "array", "items": g.fieldSchema(t.Elem(), directives)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": g.fieldSchema(t.Elem(), directives)}
	case reflect.Struct:
		return g.ref(t)
	case reflect.Interface:
		// A deliberately open value — the CLI profile overrides, whose
		// shape belongs to the profile being overridden.
		return map[string]any{}
	default:
		return map[string]any{}
	}
}

func applyStringDirectives(out map[string]any, directives map[string]string) {
	if v, ok := directives["enum"]; ok {
		parts := strings.Split(v, "|")
		values := make([]any, len(parts))
		for i, p := range parts {
			values[i] = p
		}
		out["enum"] = values
	}
	if v, ok := directives["pattern"]; ok {
		out["pattern"] = v
	}
}

func applyNumberDirectives(out map[string]any, directives map[string]string) {
	if v, ok := directives["min"]; ok {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			out["minimum"] = n
		}
	}
	if v, ok := directives["max"]; ok {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			out["maximum"] = n
		}
	}
}

// yamlName reads the authored key a field decodes from, reporting false for
// a field YAML never sees.
func yamlName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("yaml")
	if tag == "-" {
		return "", false
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		// yaml.v3 lowercases an untagged field name. Config fields are all
		// tagged; falling back keeps the schema honest if one is not.
		return strings.ToLower(f.Name), true
	}
	return name, true
}

// parseDirectives reads the js tag: semicolon-separated flags and
// key=value pairs.
//
// Semicolons rather than commas because a pattern is one of the values, and
// a character class like {0,63} contains a comma. A directive that silently
// truncated a pattern would put a WRONG rule in a public artifact, which is
// worse than no rule.
func parseDirectives(tag string) map[string]string {
	if tag == "" {
		return nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(tag, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, hasValue := strings.Cut(part, "=")
		if !hasValue {
			out[key] = ""
			continue
		}
		out[key] = value
	}
	return out
}

// cloneSchema deep-copies a fragment so a caller adding a description to
// one field's schema does not edit every other field that shares the type.
func cloneSchema(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch typed := v.(type) {
		case map[string]any:
			out[k] = cloneSchema(typed)
		case []any:
			list := make([]any, len(typed))
			for i, item := range typed {
				if m, ok := item.(map[string]any); ok {
					list[i] = cloneSchema(m)
					continue
				}
				list[i] = item
			}
			out[k] = list
		default:
			out[k] = v
		}
	}
	return out
}

// ---- the cross-field rules ------------------------------------------- //
//
// Each of these mirrors a check in the validators, and each is written to
// be SOUND: it rejects a strict subset of what the validator rejects, so an
// editor never flags a config the engine would run. The parity test asserts
// that direction over a table of documents.

// has builds the "this key is present and looks like X" shape both `if` and
// `not` clauses are made of.
func has(key string, inner map[string]any) map[string]any {
	return map[string]any{
		"properties": map[string]any{key: inner},
		"required":   []any{key},
	}
}

// constField builds a nested object schema pinning one field to a value.
func constField(field string, value any) map[string]any {
	return map[string]any{
		"properties": map[string]any{field: map[string]any{"const": value}},
		"required":   []any{field},
	}
}

// companyRules is empty, and that is a statement rather than an omission.
//
// NO CROSS-FIELD RULE THE SCHEMA CAN STATE. The company tier's one remaining
// cross-field rule — a Confluence read scope needs an integrations.confluence
// block — turns on a block's mere PRESENCE, which a JSON Schema `if` can
// express only by enumerating what "present" means for every shape the block
// can take. An editor that got that subtly wrong would red-underline a config
// the engine runs, which the package doc above calls worse than no schema at
// all. So the validator holds it alone, and this returns nothing.
//
// Kept as a function rather than dropped so the two tiers stay symmetric and
// the next expressible rule has an obvious home.
func companyRules() []any { return nil }

func bootstrapRules() []any {
	embeddedKV := has("coordination", constField("type", string(CoordinationEmbeddedKV)))

	// Local coordination holds its leases in one process, so a fleet on it
	// would have every node claim every seat. The condition is written as
	// "not explicitly embedded-kv" rather than "explicitly local" because
	// local is the DEFAULT: a config with a cluster and no coordination
	// key is the same mistake.
	noFleetOnLocal := map[string]any{
		"$comment": "local coordination cannot serve a fleet: use coordination.type embedded-kv.",
		"if":       map[string]any{"not": embeddedKV},
		"then": map[string]any{
			"properties": map[string]any{"stream": map[string]any{
				"properties": map[string]any{
					"type":    map[string]any{"const": string(StreamEmbedded)},
					"cluster": map[string]any{"properties": map[string]any{"peers": map[string]any{"maxItems": 0}}},
				},
			}},
		},
	}

	// One node or three or more. Two embedded KV members have no quorum
	// without each other, so a rolling restart makes the outage certain.
	noTwoNodeFleet := map[string]any{
		"$comment": "a two-node fleet has no coordination quorum: run one node or three or more.",
		"if":       embeddedKV,
		"then": map[string]any{
			"not": has("stream", has("cluster", has("peers",
				map[string]any{"minItems": 1, "maxItems": 1}))),
		},
	}

	replicasNeedPeers := map[string]any{
		"$comment": "stream.replicas > 1 needs peers to replicate to.",
		"if":       has("stream", has("replicas", map[string]any{"minimum": 2})),
		"then":     has("stream", has("cluster", has("peers", map[string]any{"minItems": 1}))),
	}

	externalNeedsURL := map[string]any{
		"$comment": "an external stream needs a URL to dial.",
		"if": has("stream", has("type", map[string]any{
			"enum": []any{string(StreamNATS), string(StreamPulsar)},
		})),
		"then": has("stream", has("url", map[string]any{"minLength": 1})),
	}

	return []any{noFleetOnLocal, noTwoNodeFleet, replicasNeedPeers, externalNeedsURL}
}

// SchemaTiers is every tier a schema can be generated for — what a
// `crewlet schema` command enumerates, so adding a tier reaches the CLI
// without a second list.
var SchemaTiers = []Tier{TierBootstrap, TierCompany}

// String renders a tier for a CLI flag's help text.
func (t Tier) String() string { return string(t) }

// ParseTier coerces a tier name, listing the valid ones on a miss.
func ParseTier(name string) (Tier, error) {
	for _, t := range SchemaTiers {
		if string(t) == name {
			return t, nil
		}
	}
	return "", fmt.Errorf("%w: unknown schema tier %q (want %s)",
		ErrUnknownValue, name, strings.Join(strs(SchemaTiers), " or "))
}
