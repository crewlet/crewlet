package config

import (
	"fmt"
	"maps"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/words"
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
// The engine does not read it: it is a PUBLIC, EDITOR-FACING artifact (the
// `# yaml-language-server: $schema=` modeline, a CI linter). It is a SUBSET of the validator, never a superset: an
// editor underlining a config the engine runs teaches authors to ignore it.
// EVERYTHING THE SCHEMA REJECTS, THE VALIDATOR ALSO REJECTS (the parity test);
// closed sets come from the validators' own slices. A leaf admits what the
// DECODER reads ([textTypes], [yaml11BoolPattern]): an empty value is unset, so
// every position but the company root admits null. Tier A resolves `${VAR}`
// first: a text leaf takes one anywhere, a number or switch a WHOLE one; Tier B
// keeps it verbatim, except a `pointer` field, which admits a whole one beside
// its rule. A credential's position carries [secretKeyword].
func Schema(tier Tier) ([]byte, error) {
	var root reflect.Type
	var title, id string
	var rules []any
	var rootType any

	var g *schemaGen
	// Each `$id` is where the documentation site serves the CURRENT STABLE copy
	// (/engine/latest/schema/), never a version: this tree cannot name its own
	// (the tag is the version), so a stamped file would be a second record every
	// release made stale. The site serves each version's copy under its own path
	// and stamps it, so this spelling is read only from a file outside the site.
	// latest/ is main's copy until a release of this code exists; it never serves
	// v0.1.0, whose schema describes another program. So an editor resolving this
	// `$id` gets the current release's field list, which another binary may not
	// share: the schema of the binary in hand is `crewlet schema <tier>`, which
	// the company-architect skill prefers whenever crewlet is installed.
	switch tier {
	case TierBootstrap:
		root = reflect.TypeOf(Bootstrap{})
		title = "Crewlet bootstrap config (Tier A)"
		id = "https://docs.crewlet.ai/engine/latest/schema/bootstrap.schema.json"
		g = newSchemaGen(true)
		rules = g.bootstrapRules()
		// An empty Tier A file is a node running on every default, and
		// ParseBootstrap reads a file that is nothing but a null that way.
		rootType = []any{"object", "null"}
	case TierCompany:
		root = reflect.TypeOf(Company{})
		title = "Crewlet company config (Tier B)"
		id = "https://docs.crewlet.ai/engine/latest/schema/company.schema.json"
		g = newSchemaGen(false)
		rules = companyRules()
		// A company needs at least a name, and ParseCompanyNode refuses an
		// empty document by name rather than reading it as defaults.
		rootType = "object"
	default:
		return nil, fault(nil, ErrUnknownValue, "%q isn't a schema tier. Use %s",
			string(tier), words.Or(SchemaTiers))
	}

	body := g.structSchema(root, false)
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
	// WITH ITS TEXT AS WRITTEN: a schema is read by an editor showing a
	// field's description, by a person reading the published file and by a
	// model authoring a document against it, and none of them embeds it in
	// an HTML page, the one place json.MarshalIndent's escaping of `<`, `>`
	// and `&` protects anything — so `token:<id>` was published as
	// `token:\u003cid\u003e` and every hover in an editor showed the escape.
	return plainjson.Marshal(doc, "  ")
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

// secretKeyword marks a schema position that holds a credential; its value
// is always `true`, and a position without it holds none.
//
// A VENDOR KEYWORD, because the vocabulary has no word for this. JSON Schema
// 2020-12 collects an unknown keyword as an annotation and validates nothing
// by it. The nearest standard word, `writeOnly`, says the value is never
// returned by its owner, false here: a whole ${VAR} in a credential field is
// returned verbatim on every read. SPELLED `x-crewlet-secret`: `x-` is the
// vendor convention (OpenAPI 3.1 accepts no other). A CRD schema accepts only
// its own `x-kubernetes-*` extensions, so a generator copying fragments into
// one must strip the mark and keep the policy. A boolean, because a reader
// acts only on whether a literal belongs here.
const secretKeyword = "x-crewlet-secret"

// secretDefSuffix names the $defs entry of a struct reached beneath a secret
// tag, whose strings are all credentials, beside the entry of the same struct
// reached anywhere else, whose strings are not. A dot cannot occur in the
// name of a non-generic Go type, which is every type a config document
// decodes into, so the two never collide with a third.
const secretDefSuffix = ".secret"

// yaml11BoolPattern is every string the decoder turns into a Go bool: the
// YAML 1.1 words it keeps for compatibility (`yes`, `on`, `off`…), in the
// three casings it accepts and no others.
//
// Not "true" or "false" as a QUOTED string — those the decoder refuses, and
// unquoted they are booleans already. TestTheBooleanWordsAreTheDecoders
// holds this list to the decoder's own verdict, spelling by spelling.
const yaml11BoolPattern = `^(?:y|Y|yes|Yes|YES|n|N|no|No|NO|on|On|ON|off|Off|OFF)$`

// overrides are the types the walk cannot see through, because a custom
// unmarshaler, not their fields, decides their YAML shape.
//
// Each is a hand-written fragment, and keeping the list SHORT is the point: a
// fragment is a second description of a decoder and can silently drift from
// it, so nothing else earns a custom decoder without a place here. Each is
// built from the plain-field leaf rules (a key decodes as a string, a toggle
// as a bool) and reads null as unset, so all four admit null.
//
// None is a credential's position, so none carries [secretKeyword]: a secret
// tag on one of these types would make redaction mask strings the fragment
// does not mark, which TestSchemaMarksExactlyWhatRedactionMasks fails on.
func (g *schemaGen) overrides() map[reflect.Type]map[string]any {
	text := map[string]any{"type": textTypes}
	textList := map[string]any{"type": "array", "items": text}
	toggle := g.boolSchema()

	// The mapping's keys are READ OFF PhaseLLM's own fields rather than
	// listed, because a list here is a second spelling of the decoder: a
	// phase removed from the type stayed in this list, and the schema
	// offered an editor a key the strict decoder refuses.
	phases := map[string]any{}
	llmFields := reflect.TypeFor[PhaseLLM]()
	for i := range llmFields.NumField() {
		name, _, _ := strings.Cut(llmFields.Field(i).Tag.Get("yaml"), ",")
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
		// AN INSTANT IS ONE RFC 3339 TIMESTAMP, which is what the decoder
		// reads into it: never the struct's unexported fields.
		reflect.TypeOf(time.Time{}):  {"type": textTypes, "format": "date-time"},
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

// describedFields is a config type that DERIVES the schema's description of
// some of its fields, keyed by the field's YAML name: a description that names
// one of the engine's tools reads the name from its constant
// (internal/toolname), which a `desc` tag — a literal the language takes as
// written — cannot, so a rename would have left the published schema naming a
// tool that no longer exists. internal/toolname's walk refuses a struct tag
// that spells a tool name, so this is the one way to name one.
type describedFields interface {
	fieldDescriptions() map[string]string
}

// derivedDescriptions is t's [describedFields] answer, or none.
func derivedDescriptions(t reflect.Type) map[string]string {
	described, ok := reflect.Zero(t).Interface().(describedFields)
	if !ok {
		return nil
	}
	return described.fieldDescriptions()
}

// structSchema builds the object schema for a struct type. secret says the
// struct was reached beneath a secret tag, so every string in it is a
// credential, as redaction reads it.
func (g *schemaGen) structSchema(t reflect.Type, secret bool) map[string]any {
	props := map[string]any{}
	var required []string
	derived := derivedDescriptions(t)

	// An `,inline` struct contributes its fields to the enclosing object,
	// which is what yaml.v3 does when decoding one ([inlined]) — and a tag
	// on the inlined field covers them, as it does for redaction.
	var walk func(reflect.Type, bool)
	walk = func(typ reflect.Type, secret bool) {
		for i := range typ.NumField() {
			f := typ.Field(i)
			if inlined(f) {
				walk(indirect(f.Type), secret || secrets.Field(f))
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
			schema := g.fieldSchema(f.Type, directives, secret || secrets.Field(f))
			desc, written := f.Tag.Lookup("desc")
			if text, ok := derived[name]; ok {
				if written {
					panic(fmt.Sprintf("config: %s.%s is described twice, by its "+
						"desc tag and by its type's fieldDescriptions", t.Name(), f.Name))
				}
				desc = text
				delete(derived, name)
			}
			if desc != "" {
				schema["description"] = desc
			}
			props[name] = schema
			if _, isRequired := directives["required"]; isRequired {
				required = append(required, name)
			}
		}
	}
	walk(t, secret)
	for name := range derived {
		panic(fmt.Sprintf("config: %s's fieldDescriptions describes %q, which "+
			"is none of its fields", t.Name(), name))
	}

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

// ref registers a named struct in $defs and returns a reference to it —
// under its own name, or, beneath a secret tag, under the name of its
// credential-bearing variant ([secretDefSuffix]).
func (g *schemaGen) ref(t reflect.Type, secret bool) map[string]any {
	name := t.Name()
	if name == "" {
		return g.structSchema(t, secret) // an anonymous struct has nothing to name
	}
	if secret {
		name += secretDefSuffix
	}
	if _, done := g.defs[name]; !done {
		// Reserve the name BEFORE recursing: a unit holds child units, so
		// a walk that registered on the way out would never come back.
		g.defs[name] = map[string]any{}
		g.defs[name] = g.structSchema(t, secret)
	}
	return map[string]any{"$ref": "#/$defs/" + name}
}

// fieldSchema maps one Go type onto a schema fragment: what the decoder
// reads into that type, narrowed by the field's own directives. secret says
// a secret tag covers the position, which marks every string beneath it —
// a list's members and a map's values, never a map's keys — exactly where
// redaction masks one.
func (g *schemaGen) fieldSchema(t reflect.Type, directives map[string]string, secret bool) map[string]any {
	if frag, ok := g.overridden[t]; ok {
		return cloneSchema(frag)
	}
	switch t.Kind() {
	case reflect.Pointer:
		// An optional block. Absence is expressed by the field not being
		// required, so the pointer itself adds nothing to the schema.
		return g.fieldSchema(t.Elem(), directives, secret)
	case reflect.String:
		out := g.textSchema(directives)
		if secret {
			out[secretKeyword] = true
		}
		return out
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
		// A LIST'S DIRECTIVES ARE ITS MEMBERS': a list has no shape of its
		// own a tag could state, so `js:"enum=…"` on a list of a closed set —
		// a protection rule's approvals_unless — holds each member to it, as
		// the validator holds each one.
		return map[string]any{"type": []any{"array", "null"}, "items": g.fieldSchema(t.Elem(), directives, secret)}
	case reflect.Map:
		// A MAP FIELD'S DIRECTIVES ARE ITS VALUES' RULE: a key is any text
		// the decoder reads, while a value can be a closed set (a tool's
		// level by its name), which the tag on the field is the one place
		// to state. All but `maxkeys`, the one directive about the map
		// itself: how many keys it may hold, which a value's own rule cannot
		// say (node.labels).
		values := directives
		out := map[string]any{"type": []any{"object", "null"}}
		if v, ok := directives["maxkeys"]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				out["maxProperties"] = n
			}
			values = maps.Clone(directives)
			delete(values, "maxkeys")
		}
		out["additionalProperties"] = g.fieldSchema(t.Elem(), values, secret)
		return out
	case reflect.Struct:
		return g.ref(t, secret)
	case reflect.Interface:
		// A deliberately open value — the harness profile overrides, whose
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
	// maxlen is a bound the validator already enforces, stated where an
	// editor can see it: a bound that lived only in a field's description
	// was prose to a schema, so an editor accepted a value `crewlet validate`
	// then refused. JSON Schema counts characters (code points), so a field
	// of free text is bounded in characters by its validator too (an MCP
	// server's description, [MaxMCPDescription]); a field whose own pattern
	// admits ASCII alone may count bytes, since there the two units agree.
	// It bounds the field's OWN spelling and never the reference beside it,
	// whose length says nothing about the value it resolves to.
	own := map[string]any{}
	if v, ok := directives["maxlen"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			own["maxLength"] = n
		}
	}
	if p, ok := directives["pattern"]; ok {
		own["pattern"] = p
		// Branches rather than one alternation, so the field's own rule
		// stays readable as itself — it is the same expression a validator
		// somewhere compiles, and a test compares the two.
		branches := []any{
			own,
			map[string]any{"const": ""}, // unset
		}
		if reference != nil {
			branches = append(branches, map[string]any{"pattern": reference["pattern"]})
		}
		out["anyOf"] = branches
		return out
	}
	if reference != nil && len(own) > 0 {
		// A BOUND WITH NO PATTERN STILL SITS ON THE FIELD'S OWN BRANCH,
		// never on the whole text: a ${VAR} Tier A admits beside it is as
		// long as its name, which says nothing about the value it resolves
		// to, and a schema that refused a long reference would flag a file
		// the engine runs.
		out["anyOf"] = []any{own, map[string]any{"pattern": reference["pattern"]}}
		return out
	}
	for k, v := range own {
		out[k] = v
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

// inlined reports whether yaml.v3 decodes f's keys as its parent's own: a
// field tagged `,inline`. Embedding is not what says so — yaml.v3 decodes an
// untagged embedded struct under its lowercased type name, like any other
// field, and encoding/json's flattening of the same field is a different
// decoder's rule.
func inlined(f reflect.StructField) bool {
	_, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	return strings.Contains(","+opts+",", ",inline,")
}

// parseDirectives reads the js tag: semicolon-separated flags and key=value
// pairs.
//
// Semicolons rather than commas because a pattern is one of the values, and
// a character class like {0,63} contains a comma; a truncated pattern would
// put a WRONG rule in a public artifact.
//
// THE KEYS THE GENERATOR READS are `required`; for a string `enum`,
// `pattern`, `maxlen` and `pointer`; for a number `min` and `max`; for a map
// `maxkeys`. A key outside that set, or a non-numeric bound, would be dropped
// without a word, so the suite refuses both (TestEveryDirectiveMeansWhatItSays).
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
// Each mirrors a validator check and is SOUND: it rejects a strict subset of
// what the validator rejects (the parity test asserts it). Two things make a
// condition sound that the obvious spelling misses:
//
//   - `properties` and `required` say nothing about a non-object, so they
//     alone MATCH a null (unset) block; [has] therefore states the object.
//   - In Tier A a reference may resolve to anything, so a condition on a value
//     decides which way an unknown reads: a rule that REFUSES when a value is
//     X fires only on a literal X; one refusing when it is NOT X treats a
//     reference as possibly X.

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

	// Every node carries a keyring, whatever its roles: every state-log
	// record is signed and verified under it. Not a `required` directive
	// on the field, because what the validator refuses is an EMPTY list
	// as much as an absent one, and a directive can say only the second.
	keyringOnEveryNode := map[string]any{
		"$comment": "every node needs secrets.keys: every state-log record is signed under it.",
		"allOf":    []any{has("secrets", has("keys", map[string]any{"minItems": 1}))},
	}

	return []any{noFleetOnLocal, noTwoNodeFleet, replicasNeedPeers, externalNeedsURL,
		keyringOnEveryNode}
}

// SchemaTiers is every tier a schema can be generated for — what a
// `crewlet schema` command enumerates, so adding a tier reaches the CLI
// without a second list.
var SchemaTiers = []Tier{TierBootstrap, TierCompany}

// String renders a tier for a CLI flag's help text.
func (t Tier) String() string { return string(t) }
