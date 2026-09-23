package pages

import (
	"reflect"
	"strings"
	"testing"
)

// EVERY FIELD A RECORD DECLARES IS ONE ITS DECODER KNOWS.
//
// A decoder here carries every name it does not know back out on the next
// encode, so a newer build's field survives an older build's rewrite. The
// known set is taken from marshalling a zero value, which leaves out every
// omitempty field, so those are listed by hand beside each shape — and a name
// left off that list is decoded into the struct AND carried as unknown. The
// record still round-trips while the field is set; the day a caller CLEARS it,
// the field is absent from the marshal and the stale carried copy is written
// back in its place, on every node that rewrites the row.
func TestEveryFieldARecordDeclaresIsKnownToItsDecoder(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		shape any
		known map[string]bool
	}{
		"container": {Container{}, containerFields},
		"page":      {Page{}, pageFields},
		"revision":  {Revision{}, revisionFields},
		"comment":   {Comment{}, commentFields},
		"claim":     {TitleClaim{}, claimFields},
		"change":    {Change{}, changeFields},
		"record":    {MutationRecord{}, recordFields},
	} {
		for _, field := range jsonNames(reflect.TypeOf(tc.shape)) {
			if !tc.known[field] {
				t.Errorf("%s: %q is a field the struct declares and its "+
					"decoder does not know — add it to the omitempty names "+
					"beside the field set, or clearing it will write the "+
					"stale copy back", name, field)
			}
		}
	}
}

// jsonNames is every name encoding/json writes for a struct type, with an
// untagged embedded struct's names promoted the way the encoder promotes them.
func jsonNames(t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		switch {
		case name == "-" || !f.IsExported():
			continue
		case f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct:
			out = append(out, jsonNames(f.Type)...)
		case name == "":
			out = append(out, f.Name)
		default:
			out = append(out, name)
		}
	}
	return out
}
