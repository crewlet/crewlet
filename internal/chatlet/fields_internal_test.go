package chatlet

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// EVERY KNOWN-FIELD SET NAMES EXACTLY THE FIELDS ITS TYPE HAS.
//
// The sets are what [decodeInto] tells a newer build's field from one of this
// build's, and they are written out by hand because a zero value does not
// marshal its omitempty fields — so both directions of a drift are silent:
//
//   - a name the type does not have is decoded into nothing and is no longer
//     carried either, so a newer build's field of that name is DROPPED on the
//     next re-encode — the lossless round trip the package doc promises,
//     broken for exactly one key. The message set went on naming `imported`
//     after the field it stood for was deleted with the importer.
//   - a name the type has and the set lacks is decoded into the struct AND
//     carried as unknown, so the next encode writes the stale carried copy
//     back over what the caller set.
//
// The type's own json tags are the answer, read the way encoding/json reads
// them — an untagged embedded struct's fields are promoted, `-` is no field.
func TestEveryKnownFieldSetIsItsTypesOwn(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		known map[string]bool
		of    any
	}{
		"the record": {recordFields, MutationRecord{}},
		"a channel":  {channelFields, Channel{}},
		"a message":  {messageFields, Message{}},
	} {
		want := jsonNames(reflect.TypeOf(tc.of))
		got := slices.Sorted(maps.Keys(tc.known))
		if !slices.Equal(got, want) {
			t.Errorf("%s's known fields are %v and its type defines %v — a name "+
				"the type lacks drops a newer build's field of that name, and a "+
				"name the set lacks writes a stale carried copy over the real "+
				"one", name, got, want)
		}
	}
}

// jsonNames is every top-level key encoding/json writes for a struct type,
// sorted.
func jsonNames(t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			out = append(out, jsonNames(f.Type)...)
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return slices.Compact(out)
}
