package gitlet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// A CODE RECORD VERSION IS CLOSED THE MOMENT A BUILD READS IT, on the tracker's
// rule (internal/worklet's TestAClosedRecordVersionGainsNoField): every version
// the table has held is listed with exactly its fields, and a row joining a
// closed version fails, because a build that already reads that version would
// apply the record without the field.
//
// Mutation: a field joining version 2 — a fifth row at gitlet.WorkingForVersion
// — red.
func TestAClosedCodeRecordVersionGainsNoField(t *testing.T) {
	t.Parallel()
	closed := map[int][]string{
		2: {"ChangeRouting.WorkingFor", "Mutation.WorkingFor", "PushEntry.Routing.WorkingFor",
			"PushEntry.WorkingFor"},
	}
	got := map[int][]string{}
	for _, field := range gitlet.VersionedFields() {
		got[field.Since] = append(got[field.Since], field.Name)
	}
	for version, want := range closed {
		fields := slices.Sorted(slices.Values(got[version]))
		if !slices.Equal(fields, want) {
			t.Errorf("record version %d holds %v and was closed holding %v; a field joining it is applied "+
				"without that field by every build that already reads %d, so stamp it at %d", version, fields,
				want, version, gitlet.RecordVersion+1)
		}
	}
	for version := range got {
		if _, known := closed[version]; !known {
			t.Errorf("record version %d is not listed as closed: list it with its fields, so the next field "+
				"cannot join it", version)
		}
	}
}
