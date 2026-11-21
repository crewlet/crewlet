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
// The sets tell [decodeInto] a newer build's field from this build's, and are
// hand-written (a zero value does not marshal omitempty fields), so drift is
// silent both ways:
//
//   - a name the type lacks is not carried, so a newer build's field of that
//     name is DROPPED on re-encode, breaking the lossless round trip.
//   - a name the type has and the set lacks is decoded AND carried as unknown,
//     so the next encode writes the stale copy over the caller's value.
//
// The answer is the type's json tags, read as encoding/json does.
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
