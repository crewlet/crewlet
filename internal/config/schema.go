package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

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
//
// # A leaf admits what the DECODER reads there, not what its Go type spells
//
// An editor judges the value YAML hands it; the engine judges the value its
// decoder makes of that same text, and the two differ in four ways a schema
// written from the Go types alone got wrong — each of them a red underline
// on a file the engine runs:
//
//   - AN EMPTY VALUE IS "UNSET". `level:` is YAML null, and the decoder
//     leaves the field as it was; every custom decoder here says so in as
//     many words. So every position admits null — except the company's root,
//     because an empty company document is refused by name. `level: ""` is
//     the same statement in a string field, whose zero value IS unset: every
//     validator judges a closed set or a pattern only on a value somebody set,
//     so a text field's rule admits the empty string beside its own values.
//   - TEXT IS TEXT, whatever YAML resolved it as. A string field receives
//     the scalar's own source characters, so `org_webhook: false` — the
//     spelling the GitHub guide uses — is the enum value "false" to the
//     engine and a boolean to an editor, and `gpu: true` is a label value.
//     See [textTypes] and [enumValues].
//   - A BOOLEAN ALSO READS YAML 1.1's WORDS. The decoder accepts `yes`, `on`,
//     `off` and the rest into a bool field, so they are admitted there; see
//     [yaml11BoolPattern].
//   - TIER A RESOLVES `${VAR}` BEFORE IT DECODES (see [ParseBootstrap]), so a
//     reference in a Tier A scalar is judged by what it will resolve to,
//     which the schema cannot know: it is admitted wherever SOME resolved
//     value could be decoded. A text leaf takes a reference anywhere in its
//     value, because the resolver splices the value in as text. A number or
//     a switch takes a WHOLE one, whose value is read as the same characters
//     written there would be ([Resolver.Document]); the resolver refuses a
//     reference with other text around it in those, and so does the schema.
//     Tier B keeps a reference VERBATIM, so there it is the literal text it
//     looks like and is judged as such — except in a field whose js tag says
//     `pointer`: one its consumer resolves when it is ONE WHOLE reference
//     ([envref.Resolve]) and holds to its enum or pattern only when it is
//     written out. That field admits exactly a whole reference beside its
//     rule, which is what its validator accepts (see [pointerOrLiteral]).
//
// What the schema does NOT model is a leniency that loses what the author
// wrote, because there is none left to model: a fractional number in an
// integer field is refused by the loader rather than truncated
// ([refuseFractions]), so `integer` here is exactly what the engine reads.
func Schema(tier Tier) ([]byte, error) {
	var root reflect.Type
	var title, id string
	var rules []any
	var rootType any

	var g *schemaGen
	switch tier {
	case TierBootstrap:
		root = reflect.TypeOf(Bootstrap{})
		title = "Crewlet bootstrap config (Tier A)"
		id = "https://docs.crewlet.ai/schema/bootstrap.schema.json"
		g = newSchemaGen(true)
		rules = g.bootstrapRules()
		// An empty Tier A file is a node running on every default, and
		// ParseBootstrap reads a file that is nothing but a null that way.
		rootType = []any{"object", "null"}
	case TierCompany:
		root = reflect.TypeOf(Company{})
		title = "Crewlet company config (Tier B)"
		id = "https://docs.crewlet.ai/schema/company.schema.json"
		g = newSchemaGen(false)
		rules = companyRules()
		// A company needs at least a name, and ParseCompanyNode refuses an
		// empty document by name rather than reading it as defaults.
		rootType = "object"
	default:
		return nil, fault(nil, ErrUnknownValue, "unknown schema tier %q (want %s or %s)",
			tier, TierBootstrap, TierCompany)
	}

	body := g.structSchema(root)
	body["type"] = rootType
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
// slice, a map, or one of the hand-written [schemaGen.overrides]. It once
// carried a fault channel for the one directive that could meet a shape it
// had no rule for: an integration block the build had no parser for was
// refused outright. That directive is gone with the last third-party app it
// refused (all six route end to end), so the channel has nothing left to
// report. An error return nothing can populate reads as a guard against
// something.
type schemaGen struct {
	defs map[string]map[string]any

	// resolvesReferences is whether this tier substitutes every ${VAR} in a
	// scalar BEFORE the document is decoded — Tier A's load path, and only
	// Tier A's. It is a property of the tier rather than of any field
	// because the substitution is: [Resolver.Document] rewrites every string
	// scalar in the file, so no Tier A field can opt out and none has to
	// opt in.
	resolvesReferences bool

	// overridden is [schemaGen.overrides], built once for this tier.
	overridden map[reflect.Type]map[string]any
}

func newSchemaGen(resolvesReferences bool) *schemaGen {
	g := &schemaGen{defs: map[string]map[string]any{}, resolvesReferences: resolvesReferences}
	g.overridden = g.overrides()
	return g
}

// textTypes are the JSON types a scalar can arrive as in a field that holds
// TEXT, and null.
//
// The decoder hands a string field the scalar's own source characters
// whatever YAML resolved them to, so `node.id: 7` is the id "7" and
// `gpu: true` is the label value "true". An editor sees a number and a
// boolean in the same two lines, and a schema that said "string" underlined
// both. A pattern or an enum still judges what it can see (a string); what it
// cannot see, the validator judges on the text.
var textTypes = []any{"string", "number", "boolean", "null"}

// referencePattern is the ${VAR} grammar as a JSON Schema pattern — a string
// that CARRIES a reference, anywhere in it — because the resolver expands an
// embedded reference as readily as a whole one: `edge-${ZONE}` is a node id
// once ZONE is set.
//
// It restates internal/envref's expression, which exports no source a
// generator could emit, so TestReferencePatternIsEnvrefsGrammar holds the
// two to one verdict over the cases that package's own history names. The
// pattern is deliberately unanchored: JSON Schema's `pattern` is a search,
// which is exactly "carries one".
const referencePattern = `\$\{[A-Za-z_][A-Za-z0-9_]*\}`

// wholeReferencePattern is a value that is EXACTLY one reference — what a Tier
// A number or switch takes (see [wholeReference], which the resolver judges
// by, and TestWholeReferencePatternIsTheResolvers, which holds the two to one
// verdict).
const wholeReferencePattern = `^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`

// yaml11BoolPattern is every string the decoder turns into a Go bool: the
// YAML 1.1 words it keeps for compatibility (`yes`, `on`, `off`…), in the
// three casings it accepts and no others.
//
// Not "true" or "false" as a QUOTED string — those the decoder refuses, and
// unquoted they are booleans already. TestTheBooleanWordsAreTheDecoders
// holds this list to the decoder's own verdict, spelling by spelling.
const yaml11BoolPattern = `^(?:y|Y|yes|Yes|YES|n|N|no|No|NO|on|On|ON|off|Off|OFF)$`

// overrides are the types the walk cannot see through, because their YAML
// shape is decided by a custom unmarshaler rather than by their fields.
//
// Each is a hand-written fragment. Keeping the list SHORT is the point: a
// fragment is a second description of a decoder, and every entry here is
// something that can silently drift from it. Nothing else in the package
// gets a custom decoder without earning a place in this table.
//
// Each is built from the same leaf rules as a plain field, because each of
// these decoders ends in the ordinary one: a key is decoded as a string and
// a toggle as a bool, so what those admit, these admit. And each reads a
// null as unset in its own first lines, so a null is admitted by all four.
func (g *schemaGen) overrides() map[reflect.Type]map[string]any {
	text := map[string]any{"type": textTypes}
	textList := map[string]any{"type": "array", "items": text}
	toggle := g.boolSchema()

	phases := map[string]any{}
	for _, name := range []string{"default", "review", "subagent", "auxiliary", "judge", "sandbox"} {
		phases[name] = map[string]any{"anyOf": []any{text, textList}}
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
			"anyOf":       []any{text, textList},
			"description": "A provider key, or a fallback chain of them.",
		},
		reflect.TypeOf(PhaseLLM{}): {
			"anyOf": []any{text, textList, map[string]any{
				"type":                 "object",
				"properties":           phases,
				"additionalProperties": false,
			}},
			"description": "A provider key, a fallback chain, or a per-phase mapping.",
		},
		reflect.TypeOf(ToolAnnotations{}): {
			"type":                 []any{"object", "null"},
			"properties":           annotations,
			"additionalProperties": false,
		},
	}
}

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
		// A null block is an unset one, which the decoder leaves as it was.
		// The ROOT is the exception, and [Schema] states it per tier.
		"type":       []any{"object", "null"},
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

// fieldSchema maps one Go type onto a schema fragment: what the decoder
// reads into that type, narrowed by the field's own directives.
func (g *schemaGen) fieldSchema(t reflect.Type, directives map[string]string) map[string]any {
	if frag, ok := g.overridden[t]; ok {
		return cloneSchema(frag)
	}
	switch t.Kind() {
	case reflect.Pointer:
		// An optional block. Absence is expressed by the field not being
		// required, so the pointer itself adds nothing to the schema.
		return g.fieldSchema(t.Elem(), directives)
	case reflect.String:
		return g.textSchema(directives)
	case reflect.Bool:
		return g.boolSchema()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		out := map[string]any{"type": []any{"integer", "null"}}
		applyNumberDirectives(out, directives)
		return g.orWholeReference(out)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		// The decoder refuses a negative number into an unsigned field, so
		// the floor is the type's before it is any directive's.
		out := map[string]any{"type": []any{"integer", "null"}, "minimum": 0.0}
		applyNumberDirectives(out, directives)
		return g.orWholeReference(out)
	case reflect.Float32, reflect.Float64:
		out := map[string]any{"type": []any{"number", "null"}}
		applyNumberDirectives(out, directives)
		return g.orWholeReference(out)
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": []any{"array", "null"}, "items": g.fieldSchema(t.Elem(), nil)}
	case reflect.Map:
		return map[string]any{"type": []any{"object", "null"}, "additionalProperties": g.fieldSchema(t.Elem(), nil)}
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

// textSchema is a string field: any scalar, read as its text, held to the
// field's enum or pattern where it can be seen, or empty, which is unset —
// and, in Tier A, let through wherever it carries a reference, since what
// that resolves to is decided on the node rather than in the file. A Tier B
// `pointer` field is let through as ONE WHOLE reference and nothing looser:
// its consumer resolves no other shape, so a reference inside other text is
// the literal it looks like, and the field's own rule refuses it.
func (g *schemaGen) textSchema(directives map[string]string) map[string]any {
	// The reference a field admits beside its own rule, if any.
	var reference map[string]any
	switch _, pointer := directives["pointer"]; {
	case g.resolvesReferences:
		reference = referenceSchema()
	case pointer:
		reference = wholeReferenceSchema()
	}
	if v, ok := directives["enum"]; ok {
		closed := map[string]any{"enum": enumValues(strings.Split(v, "|"))}
		if reference == nil {
			return closed
		}
		return map[string]any{"anyOf": []any{closed, reference}}
	}
	out := map[string]any{"type": textTypes}
	if p, ok := directives["pattern"]; ok {
		// Branches rather than one alternation, so the field's own rule
		// stays readable as itself — it is the same expression a validator
		// somewhere compiles, and a test compares the two.
		branches := []any{
			map[string]any{"pattern": p},
			map[string]any{"const": ""}, // unset
		}
		if reference != nil {
			branches = append(branches, map[string]any{"pattern": reference["pattern"]})
		}
		out["anyOf"] = branches
	}
	return out
}

// boolSchema is a bool field: a boolean, YAML 1.1's words for one, null for
// unset — and, in Tier A, a whole reference, whose value the resolver reads
// as the same characters written there: `true`, `false` or one of those words.
func (g *schemaGen) boolSchema() map[string]any {
	out := map[string]any{"type": []any{"boolean", "string", "null"}}
	if g.resolvesReferences {
		out["anyOf"] = []any{
			map[string]any{"pattern": yaml11BoolPattern},
			map[string]any{"pattern": wholeReferencePattern},
		}
		return out
	}
	out["pattern"] = yaml11BoolPattern
	return out
}

// orWholeReference is a number fragment that, in Tier A, also takes a whole
// reference: the resolver reads its value as the number written there (see
// [Resolver.Document]). A reference with other text around it stays refused,
// by the resolver and here alike. Tier B stores a reference verbatim, and no
// number field decodes the text "${…}", so there the fragment is unchanged.
func (g *schemaGen) orWholeReference(number map[string]any) map[string]any {
	if !g.resolvesReferences {
		return number
	}
	return map[string]any{"anyOf": []any{number, wholeReferenceSchema()}}
}

// referenceSchema is a string that carries a ${VAR} reference.
func referenceSchema() map[string]any {
	return map[string]any{"type": "string", "pattern": referencePattern}
}

// wholeReferenceSchema is a string that is exactly one ${VAR} reference.
func wholeReferenceSchema() map[string]any {
	return map[string]any{"type": "string", "pattern": wholeReferencePattern}
}

// enumValues is a closed set as an editor may hand it over: each value's own
// text, the value YAML resolves that text to when it is not a string, and
// the two spellings of unset — "" and null.
//
// The second form is the one that bites. `true` and `false` are values of
// org_webhook and group_webhook; written unquoted, as the guides write them,
// they reach an editor as booleans while the decoder hands the engine their
// text — so an enum of strings alone underlined the documented spelling.
// Each literal is resolved by the engine's own YAML reader, so the set can
// only ever gain the forms that reader produces.
func enumValues(literals []string) []any {
	values := make([]any, 0, len(literals)*2+1)
	for _, lit := range literals {
		values = append(values, lit)
	}
	for _, lit := range literals {
		var resolved any
		if err := yaml.Unmarshal([]byte(lit), &resolved); err != nil {
			continue // not a plain scalar; its text is its only form
		}
		switch resolved.(type) {
		case bool, int, int64, uint64, float64:
			values = append(values, resolved)
		}
	}
	return append(values, "", nil)
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
	for part := range strings.SplitSeq(tag, ";") {
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

// ---- the cross-field rules ------------------------------------------- //
//
// Each of these mirrors a check in the validators, and each is written to
// be SOUND: it rejects a strict subset of what the validator rejects, so an
// editor never flags a config the engine would run. The parity test asserts
// that direction over a table of documents.
//
// Two things make a condition sound that the obvious spelling misses, and
// both are about values the leaves admit:
//
//   - `properties` and `required` say nothing about a value that is not an
//     object, so a condition built from them alone MATCHES a null block —
//     and a null block is an unset one. [has] therefore states the object.
//   - In Tier A a reference may resolve to anything, so a condition that
//     turns on a value must decide which way an unknown reads. A rule that
//     REFUSES when a value is X fires only on a literal X; a rule that
//     refuses when a value is NOT X treats a reference as possibly X.

// has builds the "this key is present and looks like X" shape both `if` and
// `not` clauses are made of, on an object — see the section comment for why
// the object is stated.
func has(key string, inner map[string]any) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{key: inner},
		"required":   []any{key},
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

func (g *schemaGen) bootstrapRules() []any {
	// A literal embedded-kv, for a rule that fires ON it.
	embeddedKV := has("coordination", has("type",
		map[string]any{"const": string(CoordinationEmbeddedKV)}))
	// embedded-kv or something that may resolve to it, for a rule that fires
	// on its ABSENCE.
	mayBeEmbeddedKV := has("coordination", has("type", map[string]any{
		"anyOf": []any{
			map[string]any{"const": string(CoordinationEmbeddedKV)},
			referenceSchema(),
		},
	}))

	// Local coordination holds its leases in one process, so a fleet on it
	// would have every node claim every seat. The condition is written as
	// "not possibly embedded-kv" rather than "explicitly local" because
	// local is the DEFAULT: a config with a cluster and no coordination
	// key is the same mistake. And "possibly", because a Tier A
	// `type: "${COORDINATION}"` is a fleet's config until the node that
	// resolves it says otherwise.
	//
	// The stream half refuses a literal external NATS rather than demanding
	// a literal "embedded", for the same reason: null, a reference and an
	// absent key may all be the embedded default.
	noFleetOnLocal := map[string]any{
		"$comment": "local coordination cannot serve a fleet: use coordination.type embedded-kv.",
		"if":       map[string]any{"not": mayBeEmbeddedKV},
		"then": map[string]any{
			"properties": map[string]any{"stream": map[string]any{
				"properties": map[string]any{
					"type":    map[string]any{"not": map[string]any{"const": string(StreamNATS)}},
					"cluster": map[string]any{"properties": map[string]any{"peers": map[string]any{"maxItems": 0}}},
				},
			}},
		},
	}

	// One node or three or more. Two embedded KV members have no quorum
	// without each other, so a rolling restart makes the outage certain.
	//
	// It counts ENTRIES, which is as far as a schema can go: the validator
	// discounts an entry that is this node's own route, and recognising one
	// takes a comparison with cluster.host, cluster.port and
	// cluster.advertise that JSON Schema cannot express. So a one-entry list
	// naming only this node is flagged here — and the validator refuses that
	// list too, as naming no other member, which is what keeps this rule a
	// subset of it. A two-entry list naming this node and one other passes
	// here and is refused there, which is the safe direction.
	noTwoNodeFleet := map[string]any{
		"$comment": "a two-node fleet has no coordination quorum: run one node or three or more.",
		"if":       embeddedKV,
		"then": map[string]any{
			"not": has("stream", has("cluster", has("peers",
				map[string]any{"type": "array", "minItems": 1, "maxItems": 1}))),
		},
	}

	// Copies need members to hold them — on an EMBEDDED stream that JOINS
	// nothing, which is the only kind whose members this file names. An
	// external cluster's members stand behind stream.url and a leaf's are the
	// fleet it reaches, and the validator counts neither; a rule that
	// demanded peers of them demanded a list the validator refuses outright
	// under an external stream, so no external fleet could keep more than
	// one copy of anything without a red underline.
	// Null and "" are both unset, which the loader reads as the embedded
	// default ([Bootstrap.normalize]).
	embeddedStream := map[string]any{"anyOf": []any{
		map[string]any{"const": string(StreamEmbedded)},
		map[string]any{"const": ""},
		map[string]any{"type": "null"},
	}}
	joinsNothing := map[string]any{"anyOf": []any{
		map[string]any{"type": "null"},
		map[string]any{"type": "object", "properties": map[string]any{
			"urls": map[string]any{"anyOf": []any{
				map[string]any{"type": "null"},
				map[string]any{"type": "array", "maxItems": 0},
			}},
		}},
	}}
	replicasNeedPeers := map[string]any{
		"$comment": "stream.replicas > 1 on an embedded stream that joins no fleet needs peers to replicate to.",
		"if": has("stream", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"replicas": map[string]any{"type": "integer", "minimum": 2},
				"type":     embeddedStream,
				"leaf":     joinsNothing,
			},
			"required": []any{"replicas"},
		}),
		"then": has("stream", has("cluster", has("peers", map[string]any{"minItems": 1}))),
	}

	externalNeedsURL := map[string]any{
		"$comment": "an external stream needs a URL to dial.",
		"if":       has("stream", has("type", map[string]any{"const": string(StreamNATS)})),
		"then":     has("stream", has("url", map[string]any{"minLength": 1})),
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
