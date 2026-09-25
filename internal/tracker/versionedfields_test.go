package tracker_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// A RECORD VERSION IS CLOSED THE MOMENT A BUILD READS IT.
//
// The field table's own check (statelog's) holds the build's version to the
// table's highest, which stops a build claiming a version nothing introduced —
// but not a new field stamped at a version an EARLIER build already reads. A
// field added at 2 today is applied by every build reading 2 or 3 without it,
// because that build finds the version readable and drops the key it has no
// column for: the lossy apply the stamp exists to prevent, on the one path no
// check sees. So every version this table has ever held is listed here with
// exactly the fields it holds, and a row joining a closed version fails. A new
// field takes the next version above [tracker.RecordVersion] and adds its own
// line to this map.
func TestAClosedRecordVersionGainsNoField(t *testing.T) {
	t.Parallel()
	closed := map[int][]string{
		2: {"TurnSpend.SentBack", "TurnSpend.Workers"},
		3: {"Person.SeenThrough.Generation"},
		4: {"Comment.Choice", "Comment.Decision"},
		5: {"MutationRecord.ActorSeat"},
	}
	got := map[int][]string{}
	for _, field := range tracker.VersionedFields() {
		got[field.Since] = append(got[field.Since], field.Name)
	}
	for version, want := range closed {
		fields := slices.Sorted(slices.Values(got[version]))
		if !slices.Equal(fields, want) {
			t.Errorf("record version %d holds %v and was closed holding %v — a "+
				"field joining it is applied without that field by every build "+
				"that already reads %d; stamp it at %d", version, fields, want,
				version, tracker.RecordVersion+1)
		}
	}
	for version := range got {
		if _, known := closed[version]; !known {
			t.Errorf("record version %d is not listed as closed — list it with "+
				"its fields, so the next field cannot join it", version)
		}
	}
}
