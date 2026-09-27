// Package jsoncarrytest is what a package holds its own carrying types to:
// a value with every member set, for pinning the bytes a type writes, and a
// walk that finds an object on a shared record that does not carry.
//
// Its own package, beside [github.com/crewlet/crewlet/internal/jsoncarry],
// because only tests import it: the walk and the filler are reflection over a
// type's whole shape, which no production path has a reason to do.
package jsoncarrytest

import (
	"encoding/json"
	"reflect"
	"time"
)

// FilledAt is the instant [Filled] writes into every time it sets.
var FilledAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// Filled is a T with every member encoding/json writes set to a value that is
// not its zero one, so that no `omitempty` or `omitzero` drops it and the
// encoding shows every member the type has.
//
// DETERMINISTIC AND INDEPENDENT OF FIELD ORDER: a string is its field's own
// Go name, every integer 7, every float 1.5, every bool true, every time
// [FilledAt], every collection one element and every map the one key "key" —
// so pinned bytes move only when a member itself does. A `json.RawMessage` is
// `{"raw":true}`, because an empty one is not JSON. An interface is left nil:
// there is no one value that is right for any interface. A field the encoder
// skips — unexported, or tagged `json:"-"` — is left at its zero value, which
// is what keeps a carry's Extra empty. A struct that contains itself is
// filled once on each path through it, and left at its zero value where it
// recurs, which is the only thing that ends the fill of a recursive type.
func Filled[T any]() T {
	var v T
	fill(reflect.ValueOf(&v).Elem(), "", map[reflect.Type]bool{})
	return v
}

var (
	rawType  = reflect.TypeFor[json.RawMessage]()
	timeType = reflect.TypeFor[time.Time]()
)

func fill(v reflect.Value, label string, within map[reflect.Type]bool) {
	switch v.Type() {
	case rawType:
		v.SetBytes([]byte(`{"raw":true}`))
		return
	case timeType:
		v.Set(reflect.ValueOf(FilledAt))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(label)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), label, within)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0), label, within)
	case reflect.Array:
		for i := range v.Len() {
			fill(v.Index(i), label, within)
		}
	case reflect.Map:
		key := reflect.New(v.Type().Key()).Elem()
		fill(key, "key", within)
		value := reflect.New(v.Type().Elem()).Elem()
		fill(value, label, within)
		v.Set(reflect.MakeMap(v.Type()))
		v.SetMapIndex(key, value)
	case reflect.Struct:
		if within[v.Type()] {
			return
		}
		within[v.Type()] = true
		defer delete(within, v.Type())
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() || field.Tag.Get("json") == "-" {
				continue
			}
			fill(v.Field(i), field.Name, within)
		}
	}
}
