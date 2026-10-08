package config

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// THE SCHEMA CALLS A POSITION A CREDENTIAL EXACTLY WHERE REDACTION MASKS ONE.
//
// Two readers of one tag: the config read surface masks what `secret:"true"`
// covers, and the published schema marks it with x-crewlet-secret so that
// tooling outside the engine — an editor, a CI linter, a Kubernetes operator
// refusing to render a literal secret — knows a credential's place without
// the binary. If the two ever disagree, one of them is wrong in public: a
// position redaction masks and the schema does not mark is one a consumer
// will happily fill with a literal key, and a mark redaction does not back is
// a field a consumer will refuse to write plainly for no reason.
//
// So the oracle is the REDACTOR'S OWN WALK, run, not a second reading of the
// tag: every string of a fully populated document is a literal, redaction
// masks what it masks, and the masked positions are compared with every
// position the schema marks — in both directions, by the same path grammar
// (a field's YAML key, `[]` for a list's members, `{}` for a map's values).
func TestSchemaMarksExactlyWhatRedactionMasks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tier Tier
		root reflect.Type
		// want are positions that MUST be among the masked ones, so a walk
		// that reached nothing cannot pass by finding two empty sets equal.
		want []string
	}{
		{TierCompany, reflect.TypeFor[Company](), []string{
			"providers.llm{}.api_keys[]",
			"providers.embeddings.api_key",
			"mcp_servers[].env{}",
			"mcp_servers[].headers{}",
			"roles[].mcp_env{}{}",
			"roles[].integrations.slack.signing_secret",
			"units[].mcp_env{}{}",
			"providers.sandbox.setup[].files{}",
			"integrations.confluence.token",
		}},
		{TierBootstrap, reflect.TypeFor[Bootstrap](), nil},
	} {
		t.Run(string(tc.tier), func(t *testing.T) {
			t.Parallel()
			data, err := Schema(tc.tier)
			if err != nil {
				t.Fatalf("Schema(%s): %v", tc.tier, err)
			}
			masked := maskedPositions(tc.root)
			for _, path := range tc.want {
				if !slices.Contains(masked, path) {
					t.Errorf("control: redaction did not mask %s — the walk is not "+
						"reaching the config, so agreement below would prove nothing", path)
				}
			}
			compareMarks(t, masked, markedPositions(t, data))
		})
	}
}

// A SECRET STRUCT IS A CREDENTIAL ALL THE WAY DOWN, in the schema as in
// redaction, without marking the same struct where it is NOT beneath a tag.
//
// No config field tags a whole struct, inlines one or holds an array today,
// and redaction supports each ([TestATagCoversEverythingBeneathIt],
// [TestATaggedArrayIsMaskedAndRestored]), so the generator's answer to them
// — a variant $defs entry, a tag carried through an `,inline` field in
// either direction, an array's items — is held to the same oracle over a
// type that has them, rather than waiting for the first config field that
// does to find out.
func TestASecretStructIsMarkedAllTheWayDown(t *testing.T) {
	t.Parallel()
	// Sealed is inlined beneath a tag: its keys are the parent's, and
	// credentials.
	type Sealed struct {
		Key  string
		Keys []string
	}
	// Labels is inlined WITHOUT a tag into a struct that is reached
	// beneath one, which covers it all the same.
	type Labels struct {
		Note string
	}
	type Vault struct {
		Labels `yaml:",inline"`
		Code   string
	}
	type exposed struct {
		Plain  nestedInner
		Hidden nested
		Sealed `yaml:",inline" secret:"true"`
		Locked Vault     `secret:"true"`
		Pinned [1]string `secret:"true"`
	}
	root := reflect.TypeFor[exposed]()
	g := newSchemaGen(false)
	doc := g.structSchema(root, false)
	doc["$defs"] = g.defs
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	masked := maskedPositions(root)
	if want := []string{
		"hidden.inner.keyed{}", "hidden.inner.value", "hidden.inner.values[]",
		"key", "keys[]", "locked.code", "locked.note", "pinned[]",
	}; !slices.Equal(masked, want) {
		t.Fatalf("control: redaction masked %v, want exactly %v", masked, want)
	}
	compareMarks(t, masked, markedPositions(t, data))
}

// compareMarks reports every position one side names and the other does not.
func compareMarks(t *testing.T, masked, marked []string) {
	t.Helper()
	for _, path := range masked {
		if !slices.Contains(marked, path) {
			t.Errorf("redaction masks %s and the schema does not mark it %s: "+
				"tooling reading the schema would accept a literal credential there",
				path, secretKeyword)
		}
	}
	for _, path := range marked {
		if !slices.Contains(masked, path) {
			t.Errorf("the schema marks %s %s and redaction leaves it in the clear: "+
				"the two read one tag and must name one set", path, secretKeyword)
		}
	}
}

// maskedPositions is every position redaction masks in a document of root's
// type whose every string, list and map is populated with a literal.
//
// A type already being filled on the way down is left empty where it recurs
// (a unit's child units), which is where [markedPositions] stops following
// the same type's $defs entry, so the two walks cover the same tree.
func maskedPositions(root reflect.Type) []string {
	full := reflect.New(root).Elem()
	fillLiterals(full, map[reflect.Type]bool{})
	redacted := reflect.New(root).Elem()
	copyMasking(full, redacted, false)
	var out []string
	collectMasked(redacted, "", &out)
	slices.Sort(out)
	return slices.Compact(out)
}

// fillLiterals populates every string, pointer, list and map a config author
// can write beneath v with one literal member.
func fillLiterals(v reflect.Value, filling map[reflect.Type]bool) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("literal")
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillLiterals(p.Elem(), filling)
		v.Set(p)
	case reflect.Struct:
		if filling[v.Type()] {
			return
		}
		filling[v.Type()] = true
		defer delete(filling, v.Type())
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			if _, authored := yamlName(field); !authored {
				continue
			}
			fillLiterals(v.Field(i), filling)
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillLiterals(s.Index(0), filling)
		v.Set(s)
	case reflect.Array:
		for i := range v.Len() {
			fillLiterals(v.Index(i), filling)
		}
	case reflect.Map:
		key := reflect.New(v.Type().Key()).Elem()
		fillLiterals(key, filling)
		member := reflect.New(v.Type().Elem()).Elem()
		fillLiterals(member, filling)
		m := reflect.MakeMapWithSize(v.Type(), 1)
		m.SetMapIndex(key, member)
		v.Set(m)
	}
}

// collectMasked records the path of every string in v that redaction
// replaced with [Redacted].
func collectMasked(v reflect.Value, path string, out *[]string) {
	switch v.Kind() {
	case reflect.String:
		if v.String() == Redacted {
			*out = append(*out, path)
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			collectMasked(v.Elem(), path, out)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			// An `,inline` struct's keys are its parent's, as the decoder
			// and the generator both read them.
			if inlined(field) {
				collectMasked(v.Field(i), path, out)
				continue
			}
			name, authored := yamlName(field)
			if !authored {
				continue
			}
			collectMasked(v.Field(i), joinPosition(path, name), out)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			collectMasked(v.Index(i), path+"[]", out)
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			collectMasked(iter.Value(), path+"{}", out)
		}
	}
}

// markedPositions is every position the schema marks with [secretKeyword],
// walked from the root through properties, items, additionalProperties and
// $ref — stopping where a $defs entry recurs into one of its own struct type,
// as [maskedPositions] stops filling one.
//
// It also fails on a mark that walk cannot reach (inside a rule, a branch or
// a $defs entry nothing references), and on one whose value is not `true`:
// a mark a structural reader never meets is a claim nobody can act on.
func markedPositions(t *testing.T, data []byte) []string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	defs, _ := doc["$defs"].(map[string]any)

	var out []string
	reached := map[string]bool{}
	var walk func(node map[string]any, path, pointer string, open map[string]bool)
	walk = func(node map[string]any, path, pointer string, open map[string]bool) {
		if _, marked := node[secretKeyword]; marked {
			out = append(out, path)
			reached[pointer] = true
		}
		if ref, ok := node["$ref"].(string); ok {
			name := strings.TrimPrefix(ref, "#/$defs/")
			// The struct TYPE is what recurs, whichever variant of its
			// entry the reference names.
			typ := strings.TrimSuffix(name, secretDefSuffix)
			def, found := defs[name].(map[string]any)
			if !found {
				t.Errorf("%s: $ref %s names no $defs entry", pointer, ref)
			} else if !open[typ] {
				open[typ] = true
				walk(def, path, "/$defs/"+name, open)
				delete(open, typ)
			}
		}
		if props, ok := node["properties"].(map[string]any); ok {
			for key, sub := range props {
				if child, isObject := sub.(map[string]any); isObject {
					walk(child, joinPosition(path, key), pointer+"/properties/"+key, open)
				}
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			walk(items, path+"[]", pointer+"/items", open)
		}
		if values, ok := node["additionalProperties"].(map[string]any); ok {
			walk(values, path+"{}", pointer+"/additionalProperties", open)
		}
	}
	walk(doc, "", "", map[string]bool{})

	var everywhere func(v any, pointer string)
	everywhere = func(v any, pointer string) {
		switch typed := v.(type) {
		case map[string]any:
			if mark, marked := typed[secretKeyword]; marked {
				if mark != true {
					t.Errorf("%s: %s is %v, and the keyword's only value is true",
						pointer, secretKeyword, mark)
				}
				if !reached[pointer] {
					t.Errorf("%s carries %s where no property, list member or map "+
						"value reaches it", pointer, secretKeyword)
				}
			}
			for key, child := range typed {
				everywhere(child, pointer+"/"+key)
			}
		case []any:
			for i, child := range typed {
				everywhere(child, pointer+"/"+strconv.Itoa(i))
			}
		}
	}
	everywhere(doc, "")

	slices.Sort(out)
	return slices.Compact(out)
}

// joinPosition appends one key to a position path.
func joinPosition(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
