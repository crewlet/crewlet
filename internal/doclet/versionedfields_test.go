package doclet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/doclet"
)

// A RECORD VERSION IS CLOSED THE MOMENT A BUILD READS IT, on the tracker's
// rule (internal/worklet's TestAClosedRecordVersionGainsNoField).
//
// The field table's own check holds the build's version to the table's
// highest, which stops a build claiming a version nothing introduced, but not
// a new field stamped at a version an EARLIER build already reads: that build
// finds the version readable and applies the record without the field. So
// every version the table has ever held is listed here with exactly the fields
// it holds, and a row joining a closed version fails. A new field takes the
// next version above [doclet.RecordVersion] and adds its own line.
func TestAClosedPagesRecordVersionGainsNoField(t *testing.T) {
	t.Parallel()
	// EMPTY, because every field the knowledge base writes is in the base
	// format: the first field to join the table takes version 2 and closes
	// it here, so the field after it cannot join a version a build reads.
	closed := map[int][]string{}
	got := map[int][]string{}
	for _, field := range doclet.VersionedFields() {
		got[field.Since] = append(got[field.Since], field.Name)
	}
	for version, want := range closed {
		fields := slices.Sorted(slices.Values(got[version]))
		if !slices.Equal(fields, want) {
			t.Errorf("record version %d holds %v and was closed holding %v; a "+
				"field joining it is applied without that field by every build "+
				"that already reads %d, so stamp it at %d", version, fields, want,
				version, doclet.RecordVersion+1)
		}
	}
	for version := range got {
		if _, known := closed[version]; !known {
			t.Errorf("record version %d is not listed as closed: list it with "+
				"its fields, so the next field cannot join it", version)
		}
	}
}
