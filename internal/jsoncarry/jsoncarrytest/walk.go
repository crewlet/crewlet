package jsoncarrytest

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Uncarried names every object reachable from the roots that a newer build
// could add a member to and this build would drop — the ones a record holding
// them cannot be shared between builds without losing.
//
// The carry is per object (see [github.com/crewlet/crewlet/internal/jsoncarry]),
// so one object on a record without it is a member erased from inside the
// record by this build's first write, however carefully the record around it
// carries. The walk follows every member encoding/json writes — through
// pointers, slices, arrays and map values, and into an untagged embedded
// struct as part of the struct that embeds it — and reports each struct it
// reaches that does not carry: a field
//
//	Extra map[string]json.RawMessage `json:"-"`
//
// with a MarshalJSON on the value and an UnmarshalJSON on the pointer. A struct
// with the field and not both methods is reported as well, since its one
// method would otherwise pass it off as a value with an encoding of its own.
//
// A struct that encodes through a method of its own and has no Extra — a
// time, a scope written as a bare string or as a list — is not reported
// itself, since its method and not its members decides what it writes. Its
// EXPORTED FIELDS ARE WALKED ALL THE SAME, whatever their tags say: the method
// writes what it chooses of them, a list of objects among them, and an object
// behind such a method is one a newer build can add a member to like any
// other. So every nested object is reached, and a field the method never
// writes is one to exempt by name, with that as the reason.
//
// The one kind of struct it passes over is one the caller names in exempt,
// each with the reason it does not carry; an exempt type the walk reaches from
// none of the roots is reported, so the list cannot outlive what it names.
func Uncarried(exempt map[reflect.Type]string, roots ...reflect.Type) []string {
	w := walker{exempt: exempt, seen: map[reflect.Type]bool{}, reached: map[reflect.Type]bool{}}
	for _, root := range roots {
		w.walk(root, root.String())
	}
	for typ := range exempt {
		if !w.reached[typ] {
			w.found = append(w.found, fmt.Sprintf("%v is exempt and is reached from none of %v", typ, roots))
		}
	}
	slices.Sort(w.found)
	return w.found
}

type walker struct {
	exempt  map[reflect.Type]string
	seen    map[reflect.Type]bool
	reached map[reflect.Type]bool
	found   []string
}

var (
	extraType   = reflect.TypeFor[map[string]json.RawMessage]()
	marshaler   = reflect.TypeFor[json.Marshaler]()
	unmarshaler = reflect.TypeFor[json.Unmarshaler]()
	textWriter  = reflect.TypeFor[encoding.TextMarshaler]()
)

func (w *walker) walk(typ reflect.Type, at string) {
	switch typ.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		w.walk(typ.Elem(), at)
		return
	case reflect.Struct:
	default:
		return
	}
	if _, isExempt := w.exempt[typ]; isExempt {
		w.reached[typ] = true
		return
	}
	if w.seen[typ] {
		return
	}
	w.seen[typ] = true
	switch {
	case carries(typ):
	case hasExtra(typ):
		// HALF A CARRY: the field is there and one of the two methods is
		// not, which a type with its own encoding would otherwise pass for.
		w.found = append(w.found, fmt.Sprintf("%v (at %s) holds an Extra it does not both keep "+
			"and write back", typ, at))
	case ownEncoding(typ):
		w.encodedFields(typ, at)
		return
	default:
		w.found = append(w.found, fmt.Sprintf("%v (at %s) does not carry what it does not know", typ, at))
	}
	w.fields(typ, at)
}

// encodedFields walks every exported field of a struct its own method writes.
// The tags are not read: they are encoding/json's, and the method is what
// decides which fields reach the wire and in what shape.
func (w *walker) encodedFields(typ reflect.Type, at string) {
	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.IsExported() {
			w.walk(field.Type, at+"."+field.Name)
		}
	}
}

// fields walks the members a struct writes, an untagged embedded struct's
// among them.
func (w *walker) fields(typ reflect.Type, at string) {
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
			w.fields(inner, at)
			continue
		}
		if !field.IsExported() {
			continue
		}
		w.walk(field.Type, at+"."+field.Name)
	}
}

// carries is whether a struct holds what it does not know and writes it back.
func carries(typ reflect.Type) bool {
	return hasExtra(typ) && typ.Implements(marshaler) && reflect.PointerTo(typ).Implements(unmarshaler)
}

// hasExtra is whether a struct has the field a carry keeps its members in.
func hasExtra(typ reflect.Type) bool {
	extra, ok := typ.FieldByName("Extra")
	return ok && extra.Type == extraType && extra.Tag.Get("json") == "-"
}

// ownEncoding is whether a struct is written by a method of its own rather
// than member by member.
func ownEncoding(typ reflect.Type) bool {
	for _, own := range []reflect.Type{marshaler, textWriter} {
		if typ.Implements(own) || reflect.PointerTo(typ).Implements(own) {
			return true
		}
	}
	return false
}
