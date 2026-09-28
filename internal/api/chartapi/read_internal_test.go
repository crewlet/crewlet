package chartapi

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// EVERY FIELD A CONTENT WRITE TAKES IS A FIELD A READ SERVES, by name.
//
// A content write is full post-state, so a client editing one field sends the
// object it read back with that field changed. A body field the view does not
// carry is therefore a field every such edit sets to empty — which is how the
// views came to erase a seat's backstory, responsibilities and guidelines and a
// unit's knowledge references on every goal edit. Held here as a walk over the
// two structs' own tags, so a field added to a body and not to its view fails
// the build rather than a company's prompts.
//
// `clear_runtime` is the one body field that is a GESTURE rather than state,
// and nothing reads one back.
func TestEveryFieldAContentWriteTakesIsOneAReadServes(t *testing.T) {
	t.Parallel()
	for _, pair := range []struct {
		name       string
		body, view any
	}{
		{"seat", seatBody{}, seatView{}},
		{"unit", unitBody{}, unitView{}},
	} {
		served := jsonNames(pair.view)
		for _, field := range jsonNames(pair.body) {
			if field == "clear_runtime" {
				continue
			}
			if !slices.Contains(served, field) {
				t.Errorf("a %s's content write takes %q and its read does not serve "+
					"it, so an edit sent back from the read clears it", pair.name, field)
			}
		}
	}
}

// jsonNames is every field name a struct marshals under.
func jsonNames(v any) []string {
	var out []string
	t := reflect.TypeOf(v)
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}
