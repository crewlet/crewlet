package builtin

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/crewlet/crewlet/internal/provenance"
)

// Which fields of an answer somebody else wrote, as the records say.
//
// A record declares a field somebody's words where the field is declared
// ([provenance.FieldTag]): a task's title, a comment's body, a change's
// excerpt. This walks an answer the way its encoding will write it and finds
// every such field that holds something, so the two places a tool answer is
// encoded can hold the answer to the declaration rather than to a list of
// tools somebody keeps:
//
//   - [jsonAnswer], the plain encoder, refuses an answer carrying one at all:
//     a tool that hands a model a record holding somebody's words answers
//     through a [foreignRead], or it does not answer;
//   - [foreignRead.render] refuses one whose fenced field it did not fence,
//     or whose named field it answers with no envelope saying whose it is.
//
// WHAT IT CANNOT SEE is a string copied out of a record into an answer of the
// tool's own making — a map's value, a field of a struct with no tag. Those
// are the tool's to label, and the walk over every registered tool
// (TestEveryToolThatHandsOverSomebodysWordsIsFenced) is what finds one that
// does not.

// declaredField is one tagged field an answer carries, where its encoding
// writes it.
type declaredField struct {
	path fieldPath
	kind string
}

// declaredDepth bounds the walk: an answer is a tree a few levels deep, and a
// value deeper than this is a cycle rather than an answer.
const declaredDepth = 64

// declaredIn is every tagged field in v that holds something, in the order the
// walk met them.
func declaredIn(v any) ([]declaredField, error) {
	var out []declaredField
	err := walkDeclared(reflect.ValueOf(v), nil, &out, 0)
	return out, err
}

func walkDeclared(v reflect.Value, path fieldPath, out *[]declaredField, depth int) error {
	if depth > declaredDepth {
		return fmt.Errorf("builtin: an answer nests deeper than %d at %q", declaredDepth,
			readablePath(path.id()))
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return walkDeclared(v.Elem(), path, out, depth+1)
	case reflect.Struct:
		if encodesItself(v.Type()) {
			return nil
		}
		return walkStruct(v, path, out, depth)
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			key, err := mapKey(iter.Key())
			if err != nil {
				return err
			}
			if err := walkDeclared(iter.Value(), path.key(key), out, depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i := 0; i < v.Len(); i++ {
			if err := walkDeclared(v.Index(i), path.index(i), out, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// walkStruct walks a struct's fields by encoding/json's own rules: an
// unexported field is not written, `json:"-"` is not written, a field is named
// by its tag or else its Go name, and an embedded struct with no name of its
// own writes its fields as the outer struct's.
func walkStruct(v reflect.Value, path fieldPath, out *[]declaredField, depth int) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, written := jsonName(f)
		if !written {
			continue
		}
		fv := v.Field(i)
		if f.Anonymous && name == "" {
			inner := fv
			if inner.Kind() == reflect.Pointer {
				if inner.IsNil() {
					continue
				}
				inner = inner.Elem()
			}
			if inner.Kind() == reflect.Struct && !encodesItself(inner.Type()) {
				if err := walkStruct(inner, path, out, depth+1); err != nil {
					return err
				}
				continue
			}
			name = f.Name
		}
		if kind, tagged := f.Tag.Lookup(provenance.FieldTag); tagged {
			if holdsSomething(fv) {
				*out = append(*out, declaredField{path: path.key(name), kind: kind})
			}
			continue
		}
		if err := walkDeclared(fv, path.key(name), out, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// jsonName is the name encoding/json writes f under, and false for a field it
// does not write. An embedded field with no name of its own answers "".
func jsonName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name, _, _ := strings.Cut(tag, ",")
	if f.Anonymous {
		if !f.IsExported() && f.Type.Kind() != reflect.Struct &&
			(f.Type.Kind() != reflect.Pointer || f.Type.Elem().Kind() != reflect.Struct) {
			return "", false
		}
		return name, true
	}
	if !f.IsExported() {
		return "", false
	}
	if name == "" {
		name = f.Name
	}
	return name, true
}

// holdsSomething reports whether a tagged field carries any words: a string
// that is not empty, a list holding a string that is not, a map or raw value
// that is not empty.
func holdsSomething(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return v.Len() > 0
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && holdsSomething(v.Elem())
	case reflect.Map:
		return v.Len() > 0
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			raw := strings.TrimSpace(string(v.Bytes()))
			return raw != "" && raw != "null" && raw != `""`
		}
		for i := 0; i < v.Len(); i++ {
			if holdsSomething(v.Index(i)) {
				return true
			}
		}
	}
	return false
}

// mapKey is a map key as encoding/json writes it: a string, a number, or a
// value that marshals itself as text.
func mapKey(k reflect.Value) (string, error) {
	if k.Kind() == reflect.String {
		return k.String(), nil
	}
	if m, ok := k.Interface().(encoding.TextMarshaler); ok {
		text, err := m.MarshalText()
		if err != nil {
			return "", fmt.Errorf("builtin: a map key in an answer will not encode: %w", err)
		}
		return string(text), nil
	}
	switch k.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return fmt.Sprint(k.Interface()), nil
	}
	return "", fmt.Errorf("builtin: a map in an answer is keyed by %s", k.Type())
}

// encodesItself reports whether t writes its own encoding — a timestamp, a
// position, a raw message — whose fields are not the encoding's.
func encodesItself(t reflect.Type) bool {
	marshaler := reflect.TypeFor[json.Marshaler]()
	text := reflect.TypeFor[encoding.TextMarshaler]()
	return t.Implements(marshaler) || reflect.PointerTo(t).Implements(marshaler) ||
		t.Implements(text) || reflect.PointerTo(t).Implements(text)
}
