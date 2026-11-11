package chatlet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
)

// A RECORD VERSION IS CLOSED THE MOMENT A BUILD READS IT, on the tracker's
// rule (internal/worklet's TestAClosedRecordVersionGainsNoField): every
// version the table has held is listed with exactly its fields, and a row
// joining a closed version fails, because a build that already reads that
// version would apply the record without the field.
func TestAClosedChatRecordVersionGainsNoField(t *testing.T) {
	t.Parallel()
	closed := map[int][]string{
		2: {"MessagePost.Asks", "Notify.Answering", "Notify.Asked", "Notify.Asks", "Notify.Held"},
		3: {"MutationRecord.WorkingFor"},
	}
	got := map[int][]string{}
	for _, field := range chatlet.VersionedFields() {
		got[field.Since] = append(got[field.Since], field.Name)
	}
	for version, want := range closed {
		fields := slices.Sorted(slices.Values(got[version]))
		if !slices.Equal(fields, want) {
			t.Errorf("record version %d holds %v and was closed holding %v; a "+
				"field joining it is applied without that field by every build "+
				"that already reads %d, so stamp it at %d", version, fields, want,
				version, chatlet.RecordVersion+1)
		}
	}
	for version := range got {
		if _, known := closed[version]; !known {
			t.Errorf("record version %d is not listed as closed: list it with "+
				"its fields, so the next field cannot join it", version)
		}
	}
}
