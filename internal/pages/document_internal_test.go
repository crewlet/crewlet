package pages

import (
	"reflect"
	"strings"
	"testing"
)

// EVERY FIELD A DOCUMENT TYPE DECLARES IS IN ITS KNOWN-FIELD SET.
//
// The set is the zero value's marshalled names plus the `omitempty` names
// listed by hand beside it, and a name missing from it is decoded into the
// struct AND carried as unknown — so the next encode writes the stale carried
// copy back over what the caller set, silently. A field added with `omitempty`
// and not listed is exactly that, and nothing else notices.
func TestEveryDeclaredFieldIsKnown(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		value any
		known map[string]bool
	}{
		"Container": {Container{}, containerFields},
		"Page":      {Page{}, pageFields},
		"Revision":  {Revision{}, revisionFields},
		"Comment":   {Comment{}, commentFields},
		"Change":    {Change{}, changeFields},
	} {
		typ := reflect.TypeOf(c.value)
		for i := range typ.NumField() {
			tag, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if tag == "" || tag == "-" {
				continue
			}
			if !c.known[tag] {
				t.Errorf("%s declares %q and its known-field set does not name it "+
					"— a decode would carry it as unknown and the next encode would "+
					"write the stale copy back", name, tag)
			}
		}
		for tag := range c.known {
			if _, declared := fieldByTag(typ, tag); !declared {
				t.Errorf("%s's known-field set names %q, which the type does not "+
					"declare", name, tag)
			}
		}
	}
}

// fieldByTag finds the field whose JSON name is tag.
func fieldByTag(typ reflect.Type, tag string) (reflect.StructField, bool) {
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == tag {
			return typ.Field(i), true
		}
	}
	return reflect.StructField{}, false
}
