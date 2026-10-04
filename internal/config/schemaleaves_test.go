package config

import (
	"bytes"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/envref"
)

// probeVar is the one variable a reference probe names.
const probeVar = "CREWLET_SCHEMA_PROBE"

// literalProbes are the scalars every position is offered, as they would be
// written in a file. Each is a way the decoder's reading and an editor's
// differ: null and "" are unset, 7 and true reach a string field as their
// text, `yes` reaches a bool field as true, x is an ordinary word.
//
// And 3.7, a FRACTION: a number field that takes fractions must admit it, a
// text field reads it as its text, and an integer field refuses it at load
// rather than truncating it to 3 — which is what lets the schema keep saying
// `integer` there without underlining a value the engine runs.
var literalProbes = []string{`~`, `""`, `7`, `3.7`, `true`, `yes`, `x`}

// resolvedProbes are the values a Tier A reference is resolved to when the
// test asks whether ANY value could be decoded at a position: one of each
// kind a decoder might take — unset, a word, a whole number's text, a
// fraction's, a boolean's, a YAML 1.1 boolean word and a duration's text.
var resolvedProbes = []string{"", "x", "7", "3.7", "true", "yes", "5s"}

// emptyVar is a variable every probe environment sets to the empty string.
const emptyVar = "CREWLET_SCHEMA_EMPTY"

// referenceProbes are the two ways a Tier A value carries a reference: as the
// whole value, and beside other text. A text field takes both; a number or a
// switch takes only the first, whose value is read as if written there.
//
// The other text is a SECOND reference that resolves to nothing, so the
// spliced value is exactly the probe's — the question is whether the field
// takes a reference that is not the whole value, not whether some prefix
// happens to spell one of its words.
var referenceProbes = []string{"${" + probeVar + "}", "${" + probeVar + "}${" + emptyVar + "}"}

// resolvedFor is every value a reference is resolved to at one site: the
// shared set, and the field's own bounds where it states them — a number
// field whose floor is a gibibyte takes a reference to one, and no shared
// sample could hold every field's range.
func resolvedFor(site schemaSite) []string {
	out := slices.Clone(resolvedProbes)
	for _, bound := range []string{"min", "max"} {
		if v, ok := site.directives[bound]; ok {
			out = append(out, v)
		}
	}
	return out
}

// THE SUBSET INVARIANT AT EVERY POSITION OF BOTH TIERS, rather than over a
// table of documents somebody remembered to write.
//
// Each position the generator emits is offered each literal probe, and
// where the decoder reads the probe into a value the field's own rule
// accepts, the position's fragment must admit it — or the schema underlines
// a line the engine runs. Every hole this closed was one the parity table
// had no row for: an empty `level:`, `org_webhook: false` as the GitHub guide
// writes it, `enabled: yes`, a label value of `true`.
//
// In Tier A each position is ALSO offered a reference — a whole one, and one
// with text around it — and there the check is exact in both directions: the
// fragment admits each if and only if the resolver and decoder can turn it
// into some value the field accepts. A reference the schema refused where the
// engine takes one is the bug this test was written for (`node.id:
// "${HOSTNAME}"`, which the deployment guide recommends, and `api.port:
// "${API_PORT}"`, which the engine could not read at all until a resolved
// value was read as the field reads a literal); one it admitted where no value
// could ever decode — text around a reference in a port — would be a schema
// telling an operator a config works that cannot boot.
func TestEveryPositionAdmitsWhatTheDecoderReads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tier     Tier
		root     reflect.Type
		minSites int
	}{
		{TierBootstrap, reflect.TypeFor[Bootstrap](), 60},
		{TierCompany, reflect.TypeFor[Company](), 300},
	} {
		t.Run(string(tc.tier), func(t *testing.T) {
			t.Parallel()
			fragment := fragmentCompiler(t, tc.tier)
			sites := schemaSites(t, tc.root)
			// A walk that reaches nothing passes identically when every
			// position is right and when it has stopped looking.
			if len(sites) < tc.minSites {
				t.Fatalf("walked %d positions of %s, want at least %d — the walk is not reaching the config",
					len(sites), tc.root, tc.minSites)
			}

			var demanded, refAdmitted, refRefused int
			for _, site := range sites {
				schema := fragment(site.pointer)
				for _, probe := range literalProbes {
					doc := probeDocument(site.path, probe)
					value, decoded := decodeProbe(t, tc.tier, doc, nil, site)
					if !decoded || (probe != "~" && !directivesAdmit(site.directives, value)) {
						continue // refused by the engine too: the schema may say anything
					}
					demanded++
					if err := schema.Validate(scalarJSON(t, probe)); err != nil {
						t.Errorf("%s: the engine reads `%s` here and the schema refuses it:\n%v",
							site, probe, err)
					}
				}

				if tc.tier != TierBootstrap {
					continue // Tier B resolves nothing at load: a reference is its text
				}
				for _, written := range referenceProbes {
					engineCan := false
					for _, resolved := range resolvedFor(site) {
						value, decoded := decodeProbe(t, tc.tier,
							probeDocument(site.path, strconv.Quote(written)),
							map[string]string{probeVar: resolved, emptyVar: ""}, site)
						if decoded && directivesAdmit(site.directives, value) {
							engineCan = true
							break
						}
					}
					schemaErr := schema.Validate(written)
					switch {
					case engineCan && schemaErr != nil:
						t.Errorf("%s: Tier A resolves `%s` here into a value the engine takes, "+
							"and the schema refuses it:\n%v", site, written, schemaErr)
					case !engineCan && schemaErr == nil:
						t.Errorf("%s: the schema admits `%s` here, and nothing it could resolve "+
							"to decodes into this field", site, written)
					case engineCan:
						refAdmitted++
					default:
						refRefused++
					}
				}
			}

			if demanded == 0 {
				t.Fatal("no probe was ever read by the decoder — the probes are not reaching it")
			}
			if tc.tier == TierBootstrap && (refAdmitted == 0 || refRefused == 0) {
				t.Fatalf("a reference was admitted at %d positions and refused at %d: both directions "+
					"must be exercised, or the exact check is not checking", refAdmitted, refRefused)
			}
		})
	}
}

// THE REFERENCE GRAMMAR IS ENVREF'S. The schema restates it because envref
// exports no expression a generator could emit — and a restated grammar is
// exactly how envref's own history went wrong twice, so the two are held to
// one verdict over the shapes that history names: braces the resolver ignores
// (`${1}`, `${line#host=}`), an unbraced `$VAR`, a reference embedded in text.
func TestReferencePatternIsEnvrefsGrammar(t *testing.T) {
	t.Parallel()
	schemaSays := regexp.MustCompile(referencePattern)
	for _, value := range []string{
		"${A}", "${a_b1}", "${_X}", "x${A}y", "${A}${B}", "Bearer ${TOKEN}",
		"${1}", "${1A}", "${line#host=}", "${A-B}", "${A.B}", "${ A }", "${}",
		"${A", "$A", "$${A}", "{A}", "", "plain", "${Ä}",
	} {
		if got, want := schemaSays.MatchString(value), envref.Has(value); got != want {
			t.Errorf("%q: the schema's pattern says %v, envref.Has says %v", value, got, want)
		}
	}
}

// THE BOOLEAN WORDS ARE THE DECODER'S, spelling by spelling: every one the
// schema admits into a bool field decodes into a Go bool, and every string the
// decoder takes there is one the schema admits. The candidates are YAML 1.1's
// whole list and the near misses around it — the other casings, the quoted
// 1.2 words, whitespace, a prefix.
func TestTheBooleanWordsAreTheDecoders(t *testing.T) {
	t.Parallel()
	schemaSays := regexp.MustCompile(yaml11BoolPattern)
	for _, word := range []string{
		"y", "Y", "yes", "Yes", "YES", "n", "N", "no", "No", "NO",
		"on", "On", "ON", "off", "Off", "OFF",
		"true", "True", "TRUE", "false", "False", "FALSE",
		"yEs", "oN", "ye", "t", "f", "1", "0", "enabled", "", " yes", "yes ",
	} {
		var b bool
		// Quoted, so it reaches the decoder as a string — the form the
		// pattern judges.
		decodes := yaml.Unmarshal([]byte(strconv.Quote(word)), &b) == nil
		if got := schemaSays.MatchString(word); got != decodes {
			t.Errorf("%q: the schema's pattern says %v, the decoder %v", word, got, decodes)
		}
	}
}

// schemaSite is one position the generator emits: where a probe goes in a
// document, and where the position's fragment sits in the generated schema.
type schemaSite struct {
	// path is the YAML keys down to the position; "[]" is a list's only
	// item and "{}" a map's only key.
	path []string
	// pointer is the JSON pointer to the fragment, laid out as the
	// generator lays it out: a root's fields under /properties, a named
	// struct's under /$defs/<Name>/properties, an item under /items, a map
	// value under /additionalProperties.
	pointer    string
	directives map[string]string
}

func (s schemaSite) String() string { return strings.Join(s.path, ".") }

// schemaSites walks a tier's Go types as the generator does, returning every
// position below the root: each field, each list item, each map value.
func schemaSites(t *testing.T, root reflect.Type) []schemaSite {
	t.Helper()
	overrides := newSchemaGen(false).overridden
	var sites []schemaSite
	expanded := map[reflect.Type]bool{}
	defNames := map[string]reflect.Type{}

	var visit func(typ reflect.Type, path []string, pointer string, directives map[string]string)
	visit = func(typ reflect.Type, path []string, pointer string, directives map[string]string) {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		site := schemaSite{path: path, pointer: pointer, directives: directives}
		if _, ok := overrides[typ]; ok {
			// The generator hands an override its own fragment and reads
			// none of the field's directives into it, so a directive here
			// is a rule the published schema silently lacks.
			for key := range directives {
				if key != "required" {
					t.Errorf("%s: a %s directive on an override type, which the generator drops",
						site, key)
				}
			}
			sites = append(sites, site)
			return
		}
		switch typ.Kind() {
		case reflect.Interface:
			return // deliberately open; the generator says {}
		case reflect.Struct:
			base := pointer
			if len(path) > 0 {
				sites = append(sites, site)
				if name := typ.Name(); name != "" {
					// ONE NAME, ONE TYPE: $defs is keyed on the bare
					// name, so two types sharing one would publish the
					// first's shape for both.
					if prior, ok := defNames[name]; ok && prior != typ {
						t.Errorf("%s and %s are both $defs/%s", prior, typ, name)
					}
					defNames[name] = typ
					if expanded[typ] {
						return
					}
					expanded[typ] = true
					base = "/$defs/" + escapePointer(name)
				}
			}
			for _, f := range yamlFields(typ) {
				name, _ := yamlName(f)
				visit(f.Type, append(slices.Clone(path), name),
					base+"/properties/"+escapePointer(name), parseDirectives(f.Tag.Get("js")))
			}
		case reflect.Slice, reflect.Array:
			sites = append(sites, site)
			visit(typ.Elem(), append(slices.Clone(path), "[]"), pointer+"/items", nil)
		case reflect.Map:
			sites = append(sites, site)
			visit(typ.Elem(), append(slices.Clone(path), "{}"), pointer+"/additionalProperties", nil)
		default:
			sites = append(sites, site)
		}
	}
	visit(root, nil, "", nil)
	return sites
}

// yamlFields is a struct's decoded fields in declaration order, embedded
// structs flattened into their parent as yaml.v3 flattens them.
func yamlFields(typ reflect.Type) []reflect.StructField {
	var out []reflect.StructField
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, yamlFields(f.Type)...)
			continue
		}
		if f.PkgPath != "" {
			continue
		}
		if _, ok := yamlName(f); ok {
			out = append(out, f)
		}
	}
	return out
}

func escapePointer(token string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(token)
}

// probeDocument places one value at a position, in flow style so a probe
// needs no indentation arithmetic.
func probeDocument(path []string, value string) string {
	doc := value
	for i := len(path) - 1; i >= 0; i-- {
		switch path[i] {
		case "[]":
			doc = "[" + doc + "]"
		case "{}":
			doc = "{k: " + doc + "}"
		default:
			doc = "{" + path[i] + ": " + doc + "}"
		}
	}
	return doc
}

// decodeProbe runs a probe document down the tier's own load path as far as
// the decoder — Tier A's resolver first, with env as its only source — and
// returns what landed at the site.
//
// The validator is deliberately not run: a probe document carries one field,
// and every context rule it breaks (a file shape with no path, a key with no
// material) would read as the field refusing the value. What the FIELD makes
// of a value is its directive's business, judged by [directivesAdmit].
func decodeProbe(t *testing.T, tier Tier, doc string, env map[string]string, site schemaSite) (reflect.Value, bool) {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
		t.Fatalf("%s: probe document %q does not parse: %v", site, doc, err)
	}
	var target reflect.Value
	switch tier {
	case TierBootstrap:
		if _, err := NewResolver(MapSource(env)).Document(&node, reflect.TypeFor[Bootstrap]()); err != nil {
			return reflect.Value{}, false // the resolver refuses it: the field never sees it
		}
		cfg := DefaultBootstrap()
		if decodeDocument(&node, &cfg) != nil {
			return reflect.Value{}, false
		}
		target = reflect.ValueOf(&cfg).Elem()
	default:
		cfg := DefaultCompany()
		if decodeDocument(&node, &cfg) != nil {
			return reflect.Value{}, false
		}
		target = reflect.ValueOf(&cfg).Elem()
	}
	return valueAt(target, site.path), true
}

// valueAt follows a site's path into a decoded config, answering the zero
// Value where the path runs out (a nil block, an empty list).
func valueAt(v reflect.Value, path []string) reflect.Value {
	for _, seg := range path {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}
			}
			v = v.Elem()
		}
		switch seg {
		case "[]":
			if v.Len() == 0 {
				return reflect.Value{}
			}
			v = v.Index(0)
		case "{}":
			v = v.MapIndex(reflect.ValueOf("k").Convert(v.Type().Key()))
			if !v.IsValid() {
				return reflect.Value{}
			}
		default:
			v = fieldNamed(v, seg)
			if !v.IsValid() {
				return reflect.Value{}
			}
		}
	}
	for v.IsValid() && v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

// fieldNamed is the field a struct decodes a YAML key into, embedded
// structs included.
func fieldNamed(v reflect.Value, key string) reflect.Value {
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			if inner := fieldNamed(v.Field(i), key); inner.IsValid() {
				return inner
			}
			continue
		}
		if name, ok := yamlName(f); ok && f.PkgPath == "" && name == key {
			return v.Field(i)
		}
	}
	return reflect.Value{}
}

// directivesAdmit is whether a decoded value passes the field's own js
// directives, read as the validators read the rules they mirror: a closed
// set and a pattern judge a value somebody SET, so the empty string — a text
// field's unset — passes both; a bound judges a number.
func directivesAdmit(directives map[string]string, v reflect.Value) bool {
	if !v.IsValid() {
		return true // nothing landed: unset
	}
	switch v.Kind() {
	case reflect.String:
		s := v.String()
		if s == "" {
			return true
		}
		if closed, ok := directives["enum"]; ok && !slices.Contains(strings.Split(closed, "|"), s) {
			return false
		}
		if p, ok := directives["pattern"]; ok && !regexp.MustCompile(p).MatchString(s) {
			return false
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return withinBounds(directives, float64(v.Int()))
	case reflect.Float32, reflect.Float64:
		return withinBounds(directives, v.Float())
	}
	return true
}

func withinBounds(directives map[string]string, n float64) bool {
	if lo, ok := directives["min"]; ok {
		if bound, err := strconv.ParseFloat(lo, 64); err == nil && n < bound {
			return false
		}
	}
	if hi, ok := directives["max"]; ok {
		if bound, err := strconv.ParseFloat(hi, 64); err == nil && n > bound {
			return false
		}
	}
	return true
}

// scalarJSON is a probe as an editor hands it to a JSON Schema validator:
// the YAML scalar, read as YAML reads it.
func scalarJSON(t *testing.T, probe string) any {
	t.Helper()
	return asJSON(t, probe)
}

// fragmentCompiler compiles one position's fragment of a tier's generated
// schema, by its JSON pointer.
func fragmentCompiler(t *testing.T, tier Tier) func(pointer string) *jsonschema.Schema {
	t.Helper()
	data, err := Schema(tier)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	url := fmt.Sprintf("https://crewlet.test/%s-fragments.json", tier)
	if err := c.AddResource(url, doc); err != nil {
		t.Fatal(err)
	}
	return func(pointer string) *jsonschema.Schema {
		t.Helper()
		schema, err := c.Compile(url + "#" + pointer)
		if err != nil {
			t.Fatalf("no fragment at %s — the walk and the generator disagree on the layout: %v", pointer, err)
		}
		return schema
	}
}
