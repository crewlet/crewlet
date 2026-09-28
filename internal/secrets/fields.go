package secrets

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/redact"
)

// WHICH FIELDS OF A DOCUMENT HOLD A CREDENTIAL, decided in one place.
//
// # A tag on the field, and one reader of it
//
// A field that holds a credential says so where it is declared —
// `secret:"true"` — and everything that has to treat credentials differently
// reads that tag through [Field] and nothing else. A list of paths would be
// maintained by whoever remembered it existed, so the day somebody added
// `integrations.newthing.token` the surface serving the document would start
// publishing it and nothing would fail.
//
// THREE READERS, and they must never disagree. internal/config MASKS the
// company document it serves and restores the masks a write hands back.
// internal/chart SEALS every literal in a seat's or a unit's runtime half into
// this store before the record reaches the log, and restores a mask from the
// row it patches. And the chart's HTTP surface masks that half on the way out.
// Each read the tag for itself once, which is two definitions of "which
// fields are secret" waiting to part: the chart sealed a seat's address and
// nothing else, while every credential in its runtime half — `mcp_env`, the
// sandbox env, its own Slack app's token, its GitHub App's key — went onto the
// log in the clear.
//
// # Why the chart's half is walked ENCODED
//
// The chart cannot decode its own runtime half: the half is internal/org's
// shape, and the organization model is built on the chart rather than the
// other way round. So [Walk] reads the JSON against the Go type that decodes
// it — handed in by the package that owns the type — and rewrites the values
// in place, leaving every byte it did not change where it was and every key
// the type does not name untouched. Decoding into the type and encoding it
// again would have dropped a field a newer build wrote.
//
// IT MATCHES KEYS THE WAY encoding/json DOES, exact first and then without
// regard to case, because that is how the half is read back: a credential
// under `MCP_ENV` decodes into the seat's `mcp_env` exactly as one under the
// declared spelling does, and a walk that matched exactly would have left it
// in the clear for the decode to pick up.

// FieldTag is the struct tag that marks a field holding a credential. A field
// carrying `secret:"true"` holds one, and so does everything beneath it — a
// map of them, a list of them, a block of them.
const FieldTag = "secret"

// Field reports whether a struct field is tagged as holding a credential.
func Field(f reflect.StructField) bool { return f.Tag.Get(FieldTag) == "true" }

// Mask is what a credential reads as on a surface that serves it.
//
// # Only a WHOLE reference is shown
//
// A value that is exactly one `${VAR}` names a credential and carries none:
// the engine resolves it where a provider is built, so nothing it points at is
// in the document to leak, and it is the half an operator edits.
//
// Anything else is masked, including a value that merely CONTAINS a reference:
// "Bearer sk-live-${SUFFIX}" is legitimate — the resolver expands embedded
// references — and its literal half is a credential. An empty value is shown
// as empty, because it is a real setting and the mask ([redact.FieldMask]) is
// a distinctive literal precisely so the two stay apart.
func Mask(value string) string {
	if value == "" {
		return value
	}
	if _, whole := envref.Whole(value); whole {
		return value
	}
	return redact.FieldMask
}

// Identified is a list member that can name itself.
//
// A credential inside a list is found by WHO holds it rather than by where it
// sits, because a reorder must never hand one member's credential to another:
// a masked value restored by position after two provisioning steps swapped
// places is the second step running with the first one's registry token.
type Identified interface{ IdentityKey() string }

// Segment is one step into a document: a field's declared name, a map entry's
// key, or a list member — by its identity where it has a unique one, and by
// its position otherwise.
type Segment struct {
	Key string

	// Positional marks a list member named by its INDEX, because it has no
	// identity or shares one with another member. A value found there is
	// found by position, which is the one correspondence a reorder breaks.
	Positional bool
}

// Path is where a value sits inside a document, outermost step first.
type Path []Segment

// String is the path as a person reads it: `mcp_env.github.Authorization`,
// with a positional member as `[0]`.
func (p Path) String() string {
	var b strings.Builder
	for _, s := range p {
		if s.Positional {
			b.WriteString("[" + s.Key + "]")
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s.Key)
	}
	return b.String()
}

// Canonical is the path as an unambiguous key: two paths are one path exactly
// when their canonical forms are equal, which a dotted rendering cannot
// promise — a map key may itself hold a dot.
func (p Path) Canonical() string {
	var b strings.Builder
	for _, s := range p {
		if s.Positional {
			b.WriteByte('#')
		} else {
			b.WriteByte('=')
		}
		b.WriteString(strconv.Quote(s.Key))
	}
	return b.String()
}

// Positional reports whether any step of the path names a list member by its
// position rather than by who it is.
func (p Path) Positional() bool {
	return slices.ContainsFunc(p, func(s Segment) bool { return s.Positional })
}

// Visit is handed every string a credential field holds, with where it sits,
// and answers what the field holds instead — the value itself to leave it.
type Visit func(path Path, value string) (string, error)

// Walk calls visit for every string a credential field holds in raw, a JSON
// encoding of a value of type t, and returns the encoding with each answer in
// the value's place.
//
// RAW ITSELF, BYTE FOR BYTE, when no answer differed, so a caller comparing
// the result against what it handed in learns whether anything changed and a
// half nothing touched is stored as it arrived. An empty raw is returned as
// is. A raw that is not one JSON value is refused rather than guessed at: the
// half is decoded onto the running object, and a walk that skipped what it
// could not read would be a credential it did not see.
func Walk(t reflect.Type, raw json.RawMessage, visit Visit) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("secrets: read the document: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("secrets: read the document: something follows " +
			"its one value")
	}
	next, changed, err := walker{visit: visit}.walk(t, doc, nil, false)
	if err != nil {
		return nil, err
	}
	if !changed {
		return raw, nil
	}
	// NUMBERS KEEP THEIR SPELLING (json.Number) AND NOTHING IS HTML-ESCAPED,
	// so what changed is the credentials and nothing else about the half
	// reads differently. Keys come back sorted, which is encoding/json's
	// order for a map and changes nothing a decode reads.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(next); err != nil {
		return nil, fmt.Errorf("secrets: encode the document: %w", err)
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// walker is one [Walk].
type walker struct{ visit Visit }

// walk is one value of the decoded document against the type it decodes onto.
// secret says whether a field above it was tagged, which makes everything
// beneath it a credential.
func (w walker) walk(t reflect.Type, v any, path Path, secret bool) (any, bool, error) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// A TYPE WHOSE ENCODING IS ITS OWN — a custom decoder, an interface —
	// has no shape this walk can follow. Under a credential field every
	// string beneath it is taken for one; anywhere else it is left alone,
	// and [Walkable] is what keeps such a type from hiding a tagged field.
	if t == nil || opaque(t) {
		if secret {
			return w.all(v, path)
		}
		return v, false, nil
	}
	switch x := v.(type) {
	case string:
		if !secret {
			return v, false, nil
		}
		out, err := w.visit(slices.Clone(path), x)
		if err != nil {
			return nil, false, err
		}
		return out, out != x, nil
	case map[string]any:
		switch t.Kind() {
		case reflect.Struct:
			return w.object(t, x, path, secret)
		case reflect.Map:
			return w.entries(t.Elem(), x, path, secret)
		}
	case []any:
		if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			return w.list(t.Elem(), x, path, secret)
		}
	default:
		return v, false, nil
	}
	// A VALUE OF ANOTHER SHAPE than its type has is one a decode refuses,
	// so nothing reads it — but a credential field's is still walked whole,
	// because "nothing reads it" is a claim about this build.
	if secret {
		return w.all(v, path)
	}
	return v, false, nil
}

// object walks one JSON object against a struct type.
func (w walker) object(t reflect.Type, x map[string]any, path Path,
	secret bool) (any, bool, error) {

	fields := fieldsOf(t)
	changed := false
	for _, key := range sortedKeys(x) {
		f, known := matchField(fields, key)
		step := Segment{Key: key}
		ft, tagged := reflect.Type(nil), secret
		if known {
			// THE FIELD'S OWN NAME, not the key's spelling: two
			// spellings of one field are one field, which is how a
			// decode reads them.
			step.Key, ft, tagged = f.name, f.typ, secret || f.secret
		} else if !secret {
			// A KEY THE TYPE DOES NOT NAME decodes onto nothing, so it
			// is left exactly as it was — a field a newer build wrote
			// is carried, not dropped.
			continue
		}
		next, moved, err := w.walk(ft, x[key], append(path, step), tagged)
		if err != nil {
			return nil, false, err
		}
		if moved {
			x[key], changed = next, true
		}
	}
	return x, changed, nil
}

// entries walks one JSON object against a map type.
func (w walker) entries(elem reflect.Type, x map[string]any, path Path,
	secret bool) (any, bool, error) {

	changed := false
	for _, key := range sortedKeys(x) {
		next, moved, err := w.walk(elem, x[key], append(path, Segment{Key: key}), secret)
		if err != nil {
			return nil, false, err
		}
		if moved {
			x[key], changed = next, true
		}
	}
	return x, changed, nil
}

// list walks one JSON array against a slice or array type, naming each member
// by its identity where the element type has one and it is unique.
func (w walker) list(elem reflect.Type, xs []any, path Path,
	secret bool) (any, bool, error) {

	steps := memberSteps(elem, xs)
	changed := false
	for i := range xs {
		next, moved, err := w.walk(elem, xs[i], append(path, steps[i]), secret)
		if err != nil {
			return nil, false, err
		}
		if moved {
			xs[i], changed = next, true
		}
	}
	return xs, changed, nil
}

// all walks every string beneath v as a credential, for a value whose type
// this walk cannot follow.
func (w walker) all(v any, path Path) (any, bool, error) {
	switch x := v.(type) {
	case string:
		out, err := w.visit(slices.Clone(path), x)
		if err != nil {
			return nil, false, err
		}
		return out, out != x, nil
	case map[string]any:
		changed := false
		for _, key := range sortedKeys(x) {
			next, moved, err := w.all(x[key], append(path, Segment{Key: key}))
			if err != nil {
				return nil, false, err
			}
			if moved {
				x[key], changed = next, true
			}
		}
		return x, changed, nil
	case []any:
		changed := false
		for i := range x {
			next, moved, err := w.all(x[i],
				append(path, Segment{Key: strconv.Itoa(i), Positional: true}))
			if err != nil {
				return nil, false, err
			}
			if moved {
				x[i], changed = next, true
			}
		}
		return x, changed, nil
	}
	return v, false, nil
}

// memberSteps names every member of a list: by identity where the element type
// is [Identified] and the member's identity is non-empty and held by nobody
// else in the list, and by position otherwise.
//
// A DUPLICATED OR EMPTY IDENTITY IS NO IDENTITY, for the reason
// internal/config's restore gives: two members answering to one name are two
// members with nothing to tell them apart, and falling back to position only
// for them is the one case where position is certain to be wrong.
func memberSteps(elem reflect.Type, xs []any) []Segment {
	steps := make([]Segment, len(xs))
	identities := make([]string, len(xs))
	if identified(elem) {
		seen := map[string]int{}
		for i, x := range xs {
			identities[i] = identityOf(elem, x)
			seen[identities[i]]++
		}
		for i, id := range identities {
			if id != "" && seen[id] == 1 {
				steps[i] = Segment{Key: id}
				continue
			}
			identities[i] = ""
		}
	}
	for i := range steps {
		if identities[i] == "" {
			steps[i] = Segment{Key: strconv.Itoa(i), Positional: true}
		}
	}
	return steps
}

var identifiedType = reflect.TypeOf((*Identified)(nil)).Elem()

// identified reports whether a list's element type can name itself.
func identified(elem reflect.Type) bool {
	return elem.Implements(identifiedType) ||
		reflect.PointerTo(elem).Implements(identifiedType)
}

// identityOf is one member's identity, read by decoding it onto its type — the
// same decode the half is read back through — and empty where it does not
// decode.
func identityOf(elem reflect.Type, x any) string {
	body, err := json.Marshal(x)
	if err != nil {
		return ""
	}
	member := reflect.New(elem)
	if err := json.Unmarshal(body, member.Interface()); err != nil {
		return ""
	}
	if id, ok := member.Interface().(Identified); ok {
		return id.IdentityKey()
	}
	if id, ok := member.Elem().Interface().(Identified); ok {
		return id.IdentityKey()
	}
	return ""
}

// opaque reports a type whose JSON form is not its Go shape: one that decodes
// itself, or an interface.
func opaque(t reflect.Type) bool {
	if t.Kind() == reflect.Interface {
		return true
	}
	p := reflect.PointerTo(t)
	return p.Implements(jsonUnmarshaler) || p.Implements(textUnmarshaler)
}

var (
	jsonUnmarshaler = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	textUnmarshaler = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

// Walkable reports why a type cannot be walked faithfully, and nil when it
// can: a credential field beneath a type whose encoding is its own is one
// [Walk] cannot find, because it cannot follow that type's shape.
//
// THE OWNER OF A WALKED TYPE ASKS THIS IN A TEST, which is what keeps the one
// blind spot of an encoded walk from being a quiet one: a runtime half that
// grew a custom-decoded block holding a tagged field would otherwise put that
// field on the log in the clear with every walk reporting success.
func Walkable(t reflect.Type) error {
	return walkable(t, false, map[reflect.Type]bool{}, t.String())
}

func walkable(t reflect.Type, below bool, seen map[reflect.Type]bool, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if seen[t] {
		return nil
	}
	seen[t] = true
	if opaque(t) {
		if below && holdsTagged(t, map[reflect.Type]bool{}) {
			return fmt.Errorf("secrets: %s decodes itself and holds a field "+
				"tagged %s:\"true\", which a walk of its encoding cannot find",
				path, FieldTag)
		}
		return nil
	}
	switch t.Kind() {
	case reflect.Map, reflect.Slice, reflect.Array:
		return walkable(t.Elem(), true, seen, path+"[]")
	case reflect.Struct:
		for _, f := range fieldsOf(t) {
			if err := walkable(f.typ, true, seen, path+"."+f.name); err != nil {
				return err
			}
		}
	}
	return nil
}

// holdsTagged reports whether a struct type holds a tagged field anywhere
// beneath it.
func holdsTagged(t reflect.Type, seen map[reflect.Type]bool) bool {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Map ||
		t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return false
	}
	seen[t] = true
	for i := range t.NumField() {
		f := t.Field(i)
		if Field(f) || holdsTagged(f.Type, seen) {
			return true
		}
	}
	return false
}

// fieldInfo is one field of a struct as its encoding names it.
type fieldInfo struct {
	name   string
	typ    reflect.Type
	secret bool
}

var fieldCache sync.Map // reflect.Type -> []fieldInfo

// fieldsOf is every field a JSON object of type t can carry, by the name
// encoding/json gives it — an embedded struct's fields promoted into the
// outer one unless the outer declares the name itself, and `json:"-"` left
// out.
func fieldsOf(t reflect.Type) []fieldInfo {
	if cached, ok := fieldCache.Load(t); ok {
		return cached.([]fieldInfo)
	}
	var direct, promoted []fieldInfo
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			inner := f.Type
			if inner.Kind() == reflect.Pointer {
				inner = inner.Elem()
			}
			if inner.Kind() == reflect.Struct {
				for _, g := range fieldsOf(inner) {
					g.secret = g.secret || Field(f)
					promoted = append(promoted, g)
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		direct = append(direct, fieldInfo{name: name, typ: f.Type, secret: Field(f)})
	}
	out := direct
	for _, g := range promoted {
		if !slices.ContainsFunc(out, func(d fieldInfo) bool { return d.name == g.name }) {
			out = append(out, g)
		}
	}
	fieldCache.Store(t, out)
	return out
}

// matchField is the field a key decodes onto: its exact name first, and any
// field whose name matches without regard to case otherwise — encoding/json's
// own rule.
func matchField(fields []fieldInfo, key string) (fieldInfo, bool) {
	for _, f := range fields {
		if f.name == key {
			return f, true
		}
	}
	for _, f := range fields {
		if strings.EqualFold(f.name, key) {
			return f, true
		}
	}
	return fieldInfo{}, false
}

// sortedKeys is an object's keys in order, so a walk visits a document the same
// way every time it is handed it.
func sortedKeys(x map[string]any) []string {
	keys := make([]string, 0, len(x))
	for k := range x {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
