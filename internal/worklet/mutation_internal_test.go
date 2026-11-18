package worklet

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// THE RECORD'S KEYS ARE EXACTLY [knownKeys], IN BOTH DIRECTIONS. The list is
// hand-maintained beside the struct, and both drifts are silent:
//
//   - a field added and not listed is also filed in [MutationRecord.Extra], so
//     the encoder emits it twice;
//   - a key listed that no field writes is subtracted from Extra and stored
//     nowhere, so a newer build's field of that name is dropped on every relay.
//
// Over the tags rather than a populated record: a round trip would have to
// fill every `omitempty` field, and would pass when somebody forgot one.
func TestTheRecordsOwnKeysAreExactlyTheKnownSet(t *testing.T) {
	t.Parallel()
	var written []string
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		for i := range rt.NumField() {
			field := rt.Field(i)
			if field.Anonymous {
				// The encoder inlines an untagged embedded struct,
				// so the envelope's keys are this record's too.
				walk(field.Type)
				continue
			}
			tag := field.Tag.Get("json")
			name, _, _ := strings.Cut(tag, ",")
			if name == "-" {
				// Extra: the mechanism, not a key.
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
