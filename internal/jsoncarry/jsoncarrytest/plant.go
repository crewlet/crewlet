package jsoncarrytest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Planted is the member [Survives] adds inside every object that carries: a
// name no build writes, and a value whose bytes a copy through `any` would
// change — its keys out of order and an integer past 2^53.
const (
	PlantedName  = "a_newer_builds_fact"
	PlantedValue = `{"z":9007199254740993,"a":[true]}`
)

// Survives plants [PlantedName] in raw itself and inside every object in it
// that decodes into a type that carries — typ, raw's own type, says which —
// hands the result to roundTrip, and fails t for every object whose member did
// not come back byte for byte.
//
// roundTrip is the path under test: a decode and an encode, and whatever a
// read-modify-write does between them. The top object is planted in whatever
// its type, because carrying it is that path's job — a document can carry
// through the functions that read and write it rather than through methods of
// its own. roundTrip must leave every object where it found it, since the
// member is looked for at the path it was planted at.
func Survives(t testing.TB, typ reflect.Type, raw []byte, roundTrip func([]byte) ([]byte, error)) {
	t.Helper()
	var planted []path
	seeded, err := plant(typ, raw, nil, &planted)
	if err != nil {
		t.Fatalf("plant a newer build's member in %v: %v", typ, err)
	}
	out, err := roundTrip(seeded)
	if err != nil {
		t.Fatalf("round trip %v with a newer build's member in it: %v", typ, err)
	}
	for _, at := range planted {
		got, err := at.member(out)
		if err != nil {
			t.Errorf("%v%s: %v in %s", typ, at, err, out)
			continue
		}
		if !bytes.Equal(got, []byte(PlantedValue)) {
			t.Errorf("%v%s came back with %s = %s, want %s — the member a newer build "+
				"wrote there is lost to this build's write", typ, at, PlantedName, got, PlantedValue)
		}
	}
}

// path is where an object sits in a document: one step per member name, list
// index or map key taken from the top.
type path []step

type step struct {
	member string // an object's member, or
	key    string // a map's key, when keyed, or
	index  int    // a list's element
	keyed  bool
}

func (p path) then(s step) path { return append(p[:len(p):len(p)], s) }

func (p path) String() string {
	var b strings.Builder
	for _, s := range p {
		switch {
		case s.member != "":
			fmt.Fprintf(&b, ".%s", s.member)
		case s.keyed:
			fmt.Fprintf(&b, "[%q]", s.key)
		default:
			fmt.Fprintf(&b, "[%d]", s.index)
		}
	}
	return b.String()
}

// member is the planted member's bytes in the object at p.
func (p path) member(raw []byte) (json.RawMessage, error) {
	current := json.RawMessage(raw)
	for _, s := range p {
		switch {
		case s.member != "" || s.keyed:
			var members map[string]json.RawMessage
			if err := json.Unmarshal(current, &members); err != nil {
				return nil, fmt.Errorf("the object holding %s is gone: %w", p, err)
			}
			current = members[s.member+s.key]
		default:
			var elements []json.RawMessage
			if err := json.Unmarshal(current, &elements); err != nil {
				return nil, fmt.Errorf("the list holding %s is gone: %w", p, err)
			}
			if s.index >= len(elements) {
				return nil, fmt.Errorf("element %d of %d is gone", s.index, len(elements))
			}
			current = elements[s.index]
		}
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(current, &members); err != nil {
		return nil, fmt.Errorf("the object is gone: %w", err)
	}
	return members[PlantedName], nil
}

// plant adds the member inside every object of raw that decodes into a
// carrying type, recording the path of each.
func plant(typ reflect.Type, raw json.RawMessage, at path, planted *[]path) (json.RawMessage, error) {
	switch typ.Kind() {
	case reflect.Pointer:
		return plant(typ.Elem(), raw, at, planted)
	case reflect.Slice, reflect.Array:
		// A byte slice is written as a base64 string, and a nil list as
		// null: neither holds an object to plant in.
		if typ.Elem().Kind() == reflect.Uint8 || !opens(raw, '[') {
			return raw, nil
		}
		var elements []json.RawMessage
		if err := json.Unmarshal(raw, &elements); err != nil {
			return nil, err
		}
		for i := range elements {
			var err error
			if elements[i], err = plant(typ.Elem(), elements[i], at.then(step{index: i}), planted); err != nil {
				return nil, err
			}
		}
		return json.Marshal(elements)
	case reflect.Map:
		if !opens(raw, '{') {
			return raw, nil
		}
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, err
		}
		for key, value := range values {
			var err error
			if values[key], err = plant(typ.Elem(), value, at.then(step{key: key, keyed: true}), planted); err != nil {
				return nil, err
			}
		}
		return json.Marshal(values)
	case reflect.Struct:
	default:
		return raw, nil
	}
	if !carries(typ) && ownEncoding(typ) && len(at) > 0 {
		return raw, nil
	}
	if !opens(raw, '{') {
		return raw, nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	for name, field := range memberTypes(typ) {
		value, present := members[name]
		if !present {
			continue
		}
		var err error
		if members[name], err = plant(field, value, at.then(step{member: name}), planted); err != nil {
			return nil, err
		}
	}
	if carries(typ) || len(at) == 0 {
		members[PlantedName] = json.RawMessage(PlantedValue)
		*planted = append(*planted, at)
	}
	return json.Marshal(members)
}

// memberTypes is each member name a struct writes and the type written
// there, an untagged embedded struct's among them.
func memberTypes(typ reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		inner := field.Type
		if inner.Kind() == reflect.Pointer {
			inner = inner.Elem()
		}
		if field.Anonymous && name == "" && inner.Kind() == reflect.Struct && !ownEncoding(inner) {
			for embedded, t := range memberTypes(inner) {
				out[embedded] = t
			}
			continue
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		out[name] = field.Type
	}
	return out
}

// opens is whether raw is a JSON value that starts with delim — an object or
// a list, as opposed to null or a scalar.
func opens(raw json.RawMessage, delim byte) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == delim
}
