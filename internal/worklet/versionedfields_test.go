package worklet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/worklet"
)

// A RECORD VERSION IS CLOSED THE MOMENT A BUILD READS IT.
//
// statelog's check holds the build's version to the table's highest, but not
// a new field stamped at a version an EARLIER build already reads: that build
// drops the key and applies the rest, the lossy apply the stamp exists to
// prevent (package doc, "Every new field is version-gated, and every new
// derived column re-derived"). So every version is listed with exactly its
// fields; a new field takes the next version above [worklet.RecordVersion] and
// adds its own line.
func TestAClosedRecordVersionGainsNoField(t *testing.T) {
	t.Parallel()
	closed := map[int][]string{
		2:  {"MutationRecord.ActorApps"},
		3:  {"Snapshot.ThreadAwaiting", "Snapshot.ThreadOwing"},
		4:  {"MutationRecord.ActorAppsKept"},
		5:  {"Snapshot.CommentAsks", "Snapshot.MentionedAgents"},
		6:  {"MutationRecord.NoticeAsksKept"},
		7:  {"MutationRecord.NoticeWokeKept"},
		8:  {"TaskPatch.AskWatch"},
		9:  {"TaskPatch.WaitReason"},
		10: {"TaskPatch.ClearDue", "TaskPatch.ClearStart"},
		11: {"TaskPatch.ChangeSettled"},
		12: {"Comment.Withdrawal"},
		13: {"MutationRecord.WorkingFor"},
		14: {"Comment.Act"},
		15: {"TaskPatch.DeadWait", "TaskPatch.DeadWaitEnded"},
		16: {"Relation.Kind atlas_entry"},
		17: {"View.DeletedAt"},
		18: {"Comment.Removed"},
		19: {"Project.StuckAfterHours"},
		20: {"Snapshot.Flow", "Subject.Kind flow"},
		21: {"FlowRecord.Past"},
		22: {"Snapshot.ByOwner", "Snapshot.DepartureLead", "Snapshot.LeftUnassigned"},
	}
	got := map[int][]string{}
	for _, field := range worklet.VersionedFields() {
		got[field.Since] = append(got[field.Since], field.Name)
	}
	for version, want := range closed {
		fields := slices.Sorted(slices.Values(got[version]))
		if !slices.Equal(fields, want) {
			t.Errorf("record version %d holds %v and was closed holding %v — a "+
				"field joining it is applied without that field by every build "+
				"that already reads %d; stamp it at %d", version, fields, want,
				version, worklet.RecordVersion+1)
		}
	}
	for version := range got {
		if _, known := closed[version]; !known {
			t.Errorf("record version %d is not listed as closed — list it with "+
				"its fields, so the next field cannot join it", version)
		}
	}
}
