package iamdomain

import (
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/jsoncarry"
)

// EVERY OMITTED NAME IS LISTED IN ITS DOCUMENT'S FIELD SET.
//
// A document's known names are DERIVED by marshalling its zero value, and a
// field tagged omitempty or omitzero does not marshal at zero — so it has to
// be listed by hand, and a name missing from the list is decoded into the
// struct AND carried as unknown. The next encode then writes that stale
// carried copy back whenever the struct's own value is the zero one: a field
// cleared to its zero value was re-published at the value it had before, on
// every node, by the very write that cleared it.
//
// A list written beside a struct is a list nothing compares, and two fields
// had been missing from theirs with nothing saying so. This walks every
// document type's tags against its set.
func TestEveryOmittedNameIsListedInItsDocumentsFieldSet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		doc    any
		fields jsoncarry.Fields
	}{
		{Person{}, personFields},
		{Enrolled{}, enrolledFields},
		{IdentityChange{}, identityFields},
		{Invitation{}, invitationFields},
		{Session{}, sessionFields},
		{Revocation{}, revocationFields},
		{StatusChange{}, statusFields},
		{Removal{}, removalFields},
		{Sweep{}, sweepFields},
		{Invalidation{}, invalidationFields},
		{Eviction{}, evictionFields},
		{Generation{}, generationFields},
		{Credential{}, credentialFields},
	} {
		kind := reflect.TypeOf(tc.doc)
		for i := range kind.NumField() {
			tag := kind.Field(i).Tag.Get("json")
			name, options, _ := strings.Cut(tag, ",")
			if name == "" || name == "-" {
				continue
			}
			if !strings.Contains(options, "omitempty") &&
				!strings.Contains(options, "omitzero") {
				continue
			}
			if !tc.fields[name] {
				t.Errorf("%s.%s is omitted at zero and missing from its field "+
					"set, so a decode carries it as unknown and the next encode "+
					"of a zero value writes the stale copy back", kind.Name(),
					name)
			}
		}
	}
}
