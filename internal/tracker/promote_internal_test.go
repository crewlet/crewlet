package tracker

import (
	"errors"
	"testing"
)

// A PROMOTION'S MARK BESIDE ANY OTHER CHANGE TO THE CHECKLISTS IS REFUSED.
//
// The mark ([PromoteIntent]) and a checklist gesture ([ChecklistIntent]) are
// each a complete statement of what the checklists become, resolved inside
// the decide against the task it read. Resolved one after the other, the
// second would be decided over a set the first had already rewritten — so a
// patch naming both is a caller's mistake refused by name, exactly as a mark
// beside a whole checklist set is.
func TestAPromotionBesideAnotherChecklistChangeIsRefused(t *testing.T) {
	t.Parallel()
	current := Task{ID: "parent", Checklists: []Checklist{{
		ID: "launch", Name: "launch", Items: []ChecklistItem{{ID: "flag", Name: "flag"}},
	}}}
	mark := &PromoteIntent{Item: "flag", Subtask: "sub"}
	for name, patch := range map[string]TaskPatch{
		"a gesture": {Promote: mark, Checklist: &ChecklistIntent{
			Op: ChecklistSetDone, Item: "flag", Done: true}},
		"a whole set": {Promote: mark, Checklists: &current.Checklists},
	} {
		if _, _, err := settlePromote(current, patch); !errors.Is(err, ErrInvalid) {
			t.Errorf("a promotion beside %s = %v, want a content refusal", name, err)
		}
	}

	// ALONE, the mark resolves into the parent's own set.
	got, note, err := settlePromote(current, TaskPatch{Promote: mark})
	if err != nil || note != "" || got.Promote != nil || got.Checklists == nil {
		t.Fatalf("a promotion alone = (%+v, %q, %v)", got, note, err)
	}
	if to := (*got.Checklists)[0].Items[0].PromotedTo; to == nil || *to != "sub" {
		t.Errorf("the item points at %v, want sub", to)
	}
}
