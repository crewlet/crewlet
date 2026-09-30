package tracker

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// EVERY KEY THE RECORD WRITES IS IN THE KNOWN SET, IN BOTH DIRECTIONS.
//
// [knownKeys] is what the decoder subtracts before filing the rest in
// [MutationRecord.Extra], and it is hand-maintained beside a struct nobody is
// forced to keep it matching. Both ways of drifting are silent:
//
//   - A FIELD ADDED AND NOT LISTED is carried in Extra as well as in its own
//     field, so the encoder emits it twice — the struct's value and the map's,
//     with the merge keeping whichever the JSON encoder wrote last. The record
//     still decodes, so nothing anywhere says so.
//   - A KEY LISTED THAT NO FIELD WRITES is a key subtracted from Extra and
//     stored nowhere, which is the relay losing exactly what Extra exists to
//     keep: a newer build's field named the same as a retired one would be
//     dropped on every hop through this build.
//
// IN-PACKAGE AND OVER THE TAGS, because the list is about the FORMAT rather
// than about any record's values: a case that populated a record and read its
// JSON back would have to give every `omitempty` field a value, and would
// pass the day somebody added one it forgot to fill.
func TestTheRecordsOwnKeysAreExactlyTheKnownSet(t *testing.T) {
	t.Parallel()
	var written []string
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		for i := range rt.NumField() {
			field := rt.Field(i)
			if field.Anonymous {
				// THE ENVELOPE'S EIGHT are top-level keys of this
				// record too — an embedded struct with no tag is
				// inlined by the encoder — so its keys belong in
				// the same set.
				walk(field.Type)
				continue
			}
			tag := field.Tag.Get("json")
			name, _, _ := strings.Cut(tag, ",")
			if name == "-" {
				// Extra is the only one, and it is the mechanism
				// rather than a key.
				continue
			}
			if name == "" {
				t.Errorf("%s carries no json tag, so its key is its Go name — "+
					"which nothing here or in knownKeys states", field.Name)
				continue
			}
			written = append(written, name)
		}
	}
	walk(reflect.TypeFor[MutationRecord]())

	for _, key := range written {
		if !slices.Contains(knownKeys, key) {
			t.Errorf("the record writes %q and knownKeys does not list it, so "+
				"the decoder files it in Extra as well as in its own field and "+
				"the encoder emits it twice", key)
		}
	}
	for _, key := range knownKeys {
		if !slices.Contains(written, key) {
			t.Errorf("knownKeys lists %q and no field writes it, so a newer "+
				"build's field of that name is subtracted from Extra and "+
				"dropped on every relay through this build", key)
		}
	}
}
