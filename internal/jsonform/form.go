// Package jsonform is the one strict reader the artifact formats hold their
// JSON indexes to (a brand kit's `brand.json`, a deck's `deck.json`, a doc's
// `doc.json`, a design's `canvas.json`, a dashboard's `dashboard.json`), and
// the one account of each index's shape an agent reads before it writes one:
// a tool's contract is its validator's, and every fault comes back at once
// (ADR-0059).
//
// ONE SOURCE FOR THE CHECK AND THE REFERENCE. A [Form] is derived by
// reflection from the Go type the format decodes into, so the keys and types
// the strict decode admits are exactly the keys and types the typed decode
// reads, and the reference an agent is handed ([Form.Paths]) is the same tree
// again. A key added to the struct is a key the decode admits and the
// reference lists, in the same change, with nothing to keep in step by hand.
//
// EVERY STRUCTURAL FAULT IN ONE PASS. encoding/json with DisallowUnknownFields
// stops at the first unknown key or the first value of the wrong type, so an
// author with six faults learns of them one publish at a time. [Decode] walks
// the whole value, names every repeated key, unknown key and wrong type, each
// with the shape the format reads there, and decodes what remains, so the
// format's own field rules can run over it in the same pass.
package jsonform

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Type is what one value of a form is.
type Type uint8

// The types a form's value may be.
const (
	// Any is a value this reader does not hold to a shape: a type with its
	// own JSON reader, which holds it to its own grammar when the typed
	// decode runs.
	Any Type = iota
	// Object is a closed object: exactly its [Form.Fields], each at most
	// once.
	Object
	// Map is an object keyed by names the author chooses, each value an
	// [Form.Elem].
	Map
	// Array is a list of [Form.Elem].
	Array
	// String, Integer, Number and Boolean are the scalars.
	String
	Integer
	Number
	Boolean
)

// Form is the closed shape of one JSON value.
type Form struct {
	Type Type

	// Fields are an Object's keys, in the order its Go type declares them.
	Fields []Field

	// Elem is an Array's element, or a Map's value.
	Elem *Form
}

// Field is one key of an Object.
type Field struct {
	Key  string
	Form *Form

	// Nullable reports the field is a pointer, which a null clears where
	// it leaves a struct or a scalar as it was ([DecodeStored]).
	Nullable bool
}

// Of is the form of T, by the rules encoding/json decodes T with: a struct is
// an Object of its `json` keys (an embedded struct's keys inlined, a `-` key
// left out), a map a Map, a slice or array an Array, a pointer its element,
// and a type that reads its own JSON (json.Unmarshaler) Any. A type that reads
// itself from text (encoding.TextUnmarshaler) is a String, which is the one
// JSON value encoding/json hands it.
func Of[T any]() *Form {
	return of(reflect.TypeFor[T](), map[reflect.Type]*Form{})
}

var (
	jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
	rawMessage      = reflect.TypeFor[json.RawMessage]()
)

func of(t reflect.Type, seen map[reflect.Type]*Form) *Form {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if f, ok := seen[t]; ok {
		return f
	}
	pt := reflect.PointerTo(t)
	switch {
	case t == rawMessage, t.Implements(jsonUnmarshaler), pt.Implements(jsonUnmarshaler):
		return &Form{Type: Any}
	case t.Implements(textUnmarshaler), pt.Implements(textUnmarshaler):
		return &Form{Type: String}
	}
	switch t.Kind() {
	case reflect.Struct:
		f := &Form{Type: Object}
		seen[t] = f
		f.Fields = fieldsOf(t, seen)
		return f
	case reflect.Map:
		return &Form{Type: Map, Elem: of(t.Elem(), seen)}
	case reflect.Slice, reflect.Array:
		return &Form{Type: Array, Elem: of(t.Elem(), seen)}
	case reflect.String:
		return &Form{Type: String}
	case reflect.Bool:
		return &Form{Type: Boolean}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Form{Type: Integer}
	case reflect.Float32, reflect.Float64:
		return &Form{Type: Number}
	}
	return &Form{Type: Any}
}

// fieldsOf is a struct's keys as encoding/json reads them.
func fieldsOf(t reflect.Type, seen map[reflect.Type]*Form) []Field {
	var out []Field
	for i := range t.NumField() {
		sf := t.Field(i)
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if sf.Anonymous && name == "" {
			et := sf.Type
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				out = append(out, fieldsOf(et, seen)...)
				continue
			}
		}
		if !sf.IsExported() {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		form := of(sf.Type, seen)
		if slices.Contains(strings.Split(opts, ","), "string") {
			// A `,string` field is a quoted scalar: its own grammar.
			form = &Form{Type: Any}
		}
		out = append(out, Field{Key: name, Form: form, Nullable: sf.Type.Kind() == reflect.Pointer})
	}
	return out
}

// Field is the form of an Object's key, or nil when it carries none.
func (f *Form) Field(key string) *Form {
	if f == nil {
		return nil
	}
	for _, fl := range f.Fields {
		if fl.Key == key {
			return fl.Form
		}
	}
	return nil
}

// Keys are an Object's keys, in declaration order.
func (f *Form) Keys() []string {
	out := make([]string, len(f.Fields))
	for i, fl := range f.Fields {
		out[i] = fl.Key
	}
	return out
}

// Describe is the form as a refusal names it: "a string", "a whole number",
// "an object {kit, stack, family, files, licensed}", "a list of objects
// {path, weight, style}".
func (f *Form) Describe() string {
	switch f.Type {
	case Object:
		return "an object {" + strings.Join(f.Keys(), ", ") + "}"
	case Map:
		return "an object of names to " + plural(f.Elem)
	case Array:
		return "a list of " + plural(f.Elem)
	case String:
		return "a string"
	case Integer:
		return "a whole number"
	case Number:
		return "a number"
	case Boolean:
		return "true or false"
	}
	return "a value"
}

// plural is the form as a list's elements are named.
func plural(f *Form) string {
	switch f.Type {
	case Object:
		return "objects {" + strings.Join(f.Keys(), ", ") + "}"
	case Map:
		return "objects of names to " + plural(f.Elem)
	case Array:
		return "lists of " + plural(f.Elem)
	case String:
		return "strings"
	case Integer:
		return "whole numbers"
	case Number:
		return "numbers"
	case Boolean:
		return "true or false values"
	}
	return "values"
}

// Path is one key of a form at any depth, as a reference lists it: `[]` is
// every element of a list and `{}` every value of an object keyed by name
// (`type.heading.files[].weight`, `boards{}.x`).
type Path struct {
	Path string
	Form *Form
}

// Paths are every key of the form at every depth, depth first in declaration
// order: the reference's rows.
func (f *Form) Paths() []Path {
	var out []Path
	f.paths("", &out, map[*Form]bool{})
	return out
}

func (f *Form) paths(prefix string, out *[]Path, on map[*Form]bool) {
	if f == nil || on[f] {
		return
	}
	on[f] = true
	defer delete(on, f)
	switch f.Type {
	case Object:
		for _, fl := range f.Fields {
			p := fl.Key
			if prefix != "" {
				p = prefix + "." + fl.Key
			}
			*out = append(*out, Path{Path: p, Form: fl.Form})
			fl.Form.paths(p, out, on)
		}
	case Array:
		f.Elem.paths(prefix+"[]", out, on)
	case Map:
		f.Elem.paths(prefix+"{}", out, on)
	}
}

// Outline is the form as an agent reads it before writing a value of it: one
// line per key at every depth, its shape and, where rules has one, the rule
// the format holds it to (keyed by [Path.Path]).
//
// A rule for a path the form does not have is a reference promising a key the
// reader refuses, so it panics: rules are a format's static table, and the
// format's own test of its reference fails on the change that broke it.
func (f *Form) Outline(rules map[string]string) string {
	if _, unknown := f.Unruled(rules); len(unknown) > 0 {
		panic(fmt.Sprintf("jsonform: a reference rules keys its form does not have: %v", unknown))
	}
	var b strings.Builder
	for _, p := range f.Paths() {
		depth := strings.Count(p.Path, ".")
		b.WriteString(strings.Repeat("  ", depth))
		b.WriteString("- `" + p.Path + "` (" + p.Form.short() + ")")
		if rule := rules[p.Path]; rule != "" {
			b.WriteString(": " + rule)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Unruled is every path of the form rules says nothing about, and every rule
// about a path the form does not have: a format whose reference must cover
// its whole index tests both are empty, so a key added without its rule, or a
// rule left behind by a key removed, fails the build.
func (f *Form) Unruled(rules map[string]string) (missing, unknown []string) {
	have := map[string]bool{}
	for _, p := range f.Paths() {
		have[p.Path] = true
		if rules[p.Path] == "" {
			missing = append(missing, p.Path)
		}
	}
	for p := range rules {
		if !have[p] {
			unknown = append(unknown, p)
		}
	}
	slices.Sort(unknown)
	return missing, unknown
}

// short is the form's shape in a word or three, for an outline's row.
func (f *Form) short() string {
	switch f.Type {
	case Object:
		return "object"
	case Map:
		return "object of names to " + strings.TrimSuffix(plural(f.Elem), " {"+strings.Join(keysOf(f.Elem), ", ")+"}")
	case Array:
		return "list of " + strings.TrimSuffix(plural(f.Elem), " {"+strings.Join(keysOf(f.Elem), ", ")+"}")
	}
	return strings.TrimPrefix(strings.TrimPrefix(f.Describe(), "an "), "a ")
}

// keysOf is an Object's keys, or none for any other form.
func keysOf(f *Form) []string {
	if f.Type != Object {
		return nil
	}
	return f.Keys()
}

// OneOf is a closed set as a reference names it: "one of `a`, `b`, `c`".
func OneOf[T ~string](values []T) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = "`" + string(v) + "`"
	}
	return "one of " + strings.Join(parts, ", ")
}
