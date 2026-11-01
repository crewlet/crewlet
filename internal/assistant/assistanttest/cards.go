package assistanttest

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
)

// THE WALK THAT HOLDS A READ'S CARD TO ITS ROUTE'S REAL ANSWER.
//
// A card spec is a set of dotted paths (cards.Spec.Paths) the compiler cannot
// hold to anything, and the model reads a tool's result from its card, so a
// path naming a key the answer does not hold is a tool that tells the person
// nothing exists: list_artifacts read its rows from `items` while GET
// /artifacts answers `contents`. The walk here takes an answer a route's own
// handler gave and fails every path that is not in it. A suite that can serve
// a route (internal/api, internal/api/iamapi) asks it and hands the answer
// here, so no fixture typed beside the spec can repeat the spec's mistake.

// The scopes a card reads a path from, as cards.Path names them.
const (
	scopeAnswer = ""
	scopeRow    = "row"
	scopeRecord = "record"
	scopeItem   = "item"
)

// ValueTypes are the Go types of the values an answer may leave out or null
// in the fixture that asked it, by where they sit: a scope ("row", "record",
// "item") for its whole value, or a scope and the key path to a value inside
// it ("row.last_turn"). A path the answer does not show is held to the type
// there by its JSON tags, so a member a handler encodes with `omitempty`, or
// one only a running fleet fills, is still checked against what the handler
// would write. The KEYS above the value are always the answer's: only a
// value's type is declared, never a key. Never a map type: a map's keys are
// code, which only the handler shows.
type ValueTypes map[string]reflect.Type

// HoldCard holds every path spec reads to answer, the decoded body a route's
// handler gave the request a tool named name sent, and fails each one the
// answer does not hold:
//
//   - a path read from the answer is a key of it at every step (null counts,
//     an empty list is a list);
//   - a path read from a row, the record or an item is a key of at least one
//     of them, the rows expanded as cards.Build expands them.
//
// Every path but the spec's Note, which only a search that could not run
// answers: the caller holds it to such an answer with [Present].
//
// A path a row, the record or an item does not show is held to types instead
// (see [ValueTypes]); a type declared for a scope must hold every key a real
// value there carries, so a declaration naming the wrong type fails. It
// answers how many paths it held and which declarations it consulted, so a
// suite can fail one that is no longer needed.
func HoldCard(t testing.TB, name string, spec cards.Spec, answer any, types ValueTypes) (
	held int, consulted []string) {

	t.Helper()
	rows, records, items := cardScopes(spec, answer)
	scoped := map[string][]any{scopeRow: rows, scopeRecord: records, scopeItem: items}
	for scope, values := range scoped {
		if typ, ok := types[scope]; ok {
			for _, v := range values {
				if key, ok := memberMissing(typ, v); !ok {
					t.Errorf("%s declares its %s type as %v, which has no member %q its real "+
						"%s carries", name, scope, typ, key, scope)
				}
			}
		}
	}
	consult := func(key string) reflect.Type {
		typ, ok := types[key]
		if ok && !slices.Contains(consulted, key) {
			consulted = append(consulted, key)
		}
		return typ
	}
	for _, p := range spec.Paths() {
		// A NOTE IS ANSWERED ONLY BY A SEARCH THAT COULD NOT RUN, so it is the
		// caller's to hold, to the answer of one that could not.
		if p.From == scopeAnswer && p.Path == spec.Note {
			continue
		}
		held++
		if p.From == scopeAnswer {
			if !Present(answer, p.Path) {
				t.Errorf("%s reads %q from its answer, which has no such key: %.300s", name,
					p.Path, encoded(answer))
			}
			continue
		}
		values, known := scoped[p.From]
		if !known {
			t.Fatalf("%s reads %q from %q, a scope this walk does not know", name, p.Path,
				p.From)
		}
		if p.From == scopeRow && spec.Table != nil && p.Path == spec.Table.Expand {
			// THE EXPANDED LIST is read from the outer row.
			values, _ = cards.Lookup(answer, spec.Table.Rows).([]any)
		}
		if heldBy(values, p.From, p.Path, consult) {
			continue
		}
		typ := consult(p.From)
		switch {
		case typ != nil && TypeHolds(typ, p.Path):
			continue
		case typ != nil:
			t.Errorf("%s reads %q from each %s, which no real %s shows and its declared type "+
				"%v does not hold", name, p.Path, p.From, p.From, typ)
			continue
		}
		if len(values) == 0 {
			t.Errorf("%s answers no %s here and declares no %s type, so %q is held to "+
				"nothing: give its source a value, or declare its type", name, p.From,
				p.From, p.Path)
			continue
		}
		t.Errorf("%s reads %q from each %s, which none of its %d holds: %.300s", name, p.Path,
			p.From, len(values), encoded(values[0]))
	}
	return held, consulted
}

// heldBy reports whether some value holds path: to its end, or to a null whose
// declared type ([ValueTypes], at scope and the keys walked) holds the rest.
func heldBy(values []any, scope, path string, consult func(string) reflect.Type) bool {
	keys := strings.Split(path, ".")
	for _, v := range values {
		for i, key := range keys {
			obj, ok := v.(map[string]any)
			if !ok {
				break
			}
			if v, ok = obj[key]; !ok {
				break
			}
			if i == len(keys)-1 {
				return true
			}
			if v == nil {
				at := scope + "." + strings.Join(keys[:i+1], ".")
				if typ := consult(at); typ != nil && TypeHolds(typ, strings.Join(keys[i+1:], ".")) {
					return true
				}
				break
			}
		}
	}
	return false
}

// cardScopes are what a card reads its row, record and item paths from: the
// rows it draws (expanded, as cards.Build expands them), its record, and the
// items it lists.
func cardScopes(spec cards.Spec, answer any) (rows, records, items []any) {
	if tb := spec.Table; tb != nil {
		rows, _ = cards.Lookup(answer, tb.Rows).([]any)
		if tb.Expand != "" {
			var out []any
			for _, row := range rows {
				children, _ := cards.Lookup(row, tb.Expand).([]any)
				for _, child := range children {
					if fields, ok := child.(map[string]any); ok {
						merged := maps.Clone(fields)
						merged["parent"] = row
						out = append(out, merged)
					}
				}
			}
			rows = out
		}
	}
	if e := spec.Entity; e != nil {
		if record := cards.Lookup(answer, e.Root); record != nil {
			records = []any{record}
		}
		if e.Items != nil {
			items, _ = cards.Lookup(answer, e.Items.Path).([]any)
		}
	}
	return rows, records, items
}

// Present reports whether path names a key of v at every step, whatever the
// value at its end (null included): a key the answer holds and left empty is
// the card's to draw as empty, while a key it does not hold is a defect. An
// empty path is v itself.
func Present(v any, path string) bool {
	if path == "" {
		return v != nil
	}
	for _, key := range strings.Split(path, ".") {
		obj, ok := v.(map[string]any)
		if !ok {
			return false
		}
		if v, ok = obj[key]; !ok {
			return false
		}
	}
	return true
}

// TypeHolds reports whether a value of type t encodes path: each step a JSON
// member of a struct, an embedded struct's members promoted as encoding/json
// promotes them, through pointers.
func TypeHolds(t reflect.Type, path string) bool {
	for _, key := range strings.Split(path, ".") {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return false
		}
		next, ok := jsonMember(t, key)
		if !ok {
			return false
		}
		t = next
	}
	return true
}

// memberMissing is a key of v (a decoded object) that type t has no member
// for, and false; or true where t holds every key v carries. The `parent` an
// expanded row carries is the walk's, not the value's.
func memberMissing(t reflect.Type, v any) (string, bool) {
	obj, _ := v.(map[string]any)
	for _, key := range slices.Sorted(maps.Keys(obj)) {
		if key != "parent" && !TypeHolds(t, key) {
			return key, false
		}
	}
	return "", true
}

// jsonMember is the type of the member name of struct t as encoding/json
// writes it.
func jsonMember(t reflect.Type, name string) (reflect.Type, bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == "-" || (!f.IsExported() && !f.Anonymous) {
			continue
		}
		if f.Anonymous && tag == "" {
			inner := f.Type
			for inner.Kind() == reflect.Pointer {
				inner = inner.Elem()
			}
			if inner.Kind() == reflect.Struct {
				if m, ok := jsonMember(inner, name); ok {
					return m, true
				}
				continue
			}
		}
		if tag == "" {
			tag = f.Name
		}
		if tag == name {
			return f.Type, true
		}
	}
	return nil, false
}

// encoded is v as JSON, for a failure's message.
func encoded(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// CardVariants are example arguments that reach each of tool's card variants:
// [ExampleArgs], each with one of its arguments left out, and each with one
// enum argument set to each of its values; the first to reach a variant is
// kept.
func CardVariants(t testing.TB, tool catalog.Tool) map[string]map[string]any {
	t.Helper()
	base := ExampleArgs(t, tool.Parameters)
	out := map[string]map[string]any{}
	try := func(args map[string]any) {
		if _, err := tool.Request(args); err != nil {
			return
		}
		variant := ""
		if tool.Variant != nil {
			variant = tool.Variant(args)
		}
		if _, ok := out[variant]; !ok {
			out[variant] = args
		}
	}
	try(base)
	props, _ := tool.Parameters["properties"].(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(props)) {
		less := maps.Clone(base)
		delete(less, name)
		try(less)
		prop, _ := props[name].(map[string]any)
		enum, _ := prop["enum"].([]string)
		for _, value := range enum {
			with := maps.Clone(base)
			with[name] = value
			try(with)
		}
	}
	return out
}

// Filled is a T with every exported field set, for a source a suite stands in
// for to answer: a string "filled", a number 1, a flag true, an instant at,
// a UUID, a raw JSON value `{}`, a pointer to a filled value and a list or a
// map of one filled entry, to a depth that ends a recursive type. A handler
// answering it writes every member it can, so a card read over that answer
// meets every key.
func Filled[T any](at time.Time) T {
	var v T
	fill(reflect.ValueOf(&v).Elem(), at, 0)
	return v
}

var (
	timeType = reflect.TypeFor[time.Time]()
	uuidType = reflect.TypeFor[uuid.UUID]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

func fill(v reflect.Value, at time.Time, depth int) {
	if depth > 6 || !v.CanSet() {
		return
	}
	switch v.Type() {
	case timeType:
		v.Set(reflect.ValueOf(at))
		return
	case uuidType:
		v.Set(reflect.ValueOf(uuid.NewSHA1(uuid.NameSpaceOID, []byte("filled"))))
		return
	case rawType:
		v.Set(reflect.ValueOf(json.RawMessage(`{}`)))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("filled")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(p.Elem(), at, depth+1)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0), at, depth+1)
		v.Set(s)
	case reflect.Array:
		for i := range v.Len() {
			fill(v.Index(i), at, depth+1)
		}
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key, value := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(key, at, depth+1)
		fill(value, at, depth+1)
		m.SetMapIndex(key, value)
		v.Set(m)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), at, depth+1)
			}
		}
	}
}
