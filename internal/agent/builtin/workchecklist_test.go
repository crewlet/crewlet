package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN ASSIGNMENT'S REASON IS THE CHANGE'S EXCERPT, so it reaches both the wake
// the new assignee is woken with and the history row beside the hand-off —
// with no field of its own on the record, because the excerpt is already the
// line both of them carry.
func TestAReassignmentReasonReachesTheWakeAndTheHistory(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	const why = "You own the rollout now — ops has the rest."
	if got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "assignee": "ops", "reason": why,
	}); got.Failed {
		t.Fatalf("an assignment with a reason failed: %s", got.Output)
	}
	if len(trk.notified) != 1 || trk.notified[0] == nil {
		t.Fatalf("the assignment was published with %v", trk.notified)
	}
	if got := trk.notified[0].Excerpt; got != why {
		t.Errorf("the assignment's excerpt is %q, want the reason %q — the "+
			"excerpt is what the history row stores and the wake renders, so "+
			"a reason that is not on it reaches nobody", got, why)
	}
	if trk.kinds[0] != tracker.ChangeAssignee {
		t.Errorf("the change kind is %q, want assignee", trk.kinds[0])
	}
}

// A REASON EXPLAINS AN ASSIGNMENT AND NOTHING ELSE, and it is refused past its
// bounds rather than cut: a reason silently truncated reads as finished.
func TestAnAssignmentReasonIsBoundedAndOnlyBesideAnAssignee(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		args map[string]any
		want string
	}{
		"no assignee": {map[string]any{"item": "ENG-1", "status": "done",
			"reason": "shipped"}, "comment"},
		"too many characters": {map[string]any{"item": "ENG-1", "assignee": "ops",
			"reason": strings.Repeat("a", builtin.MaxAssignmentReason+1)}, "characters"},
		"too many bytes": {map[string]any{"item": "ENG-1", "assignee": "ops",
			"reason": strings.Repeat("語", builtin.MaxAssignmentReason)}, "bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
			got := callWork(t, reg, builtin.UpdateWorkItemTool, c.args)
			if !got.Failed || !strings.Contains(got.Output, c.want) {
				t.Fatalf("got %+v, want a refusal mentioning %q", got, c.want)
			}
			if len(trk.patched) != 0 {
				t.Errorf("a refused reason still wrote %v", trk.patched)
			}
		})
	}
}

// A CHECKLIST CHANGE TRAVELS AS A GESTURE, never as the collection: this tool
// read the item in one transaction and the write lands in another, so a whole
// collection formed here would discard every tick and every line somebody
// else wrote in between.
func TestAChecklistChangeIsAGestureNotACollection(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	if got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "checklist": map[string]any{
			"op": "add_list", "name": "Launch", "items": []any{"migrate", "announce"},
		},
	}); got.Failed {
		t.Fatalf("add_list failed: %s", got.Output)
	}
	patch := trk.patched[0]
	if patch.Checklists != nil {
		t.Fatalf("the tool wrote a whole checklist collection %v — it can only "+
			"have formed it from its own read", *patch.Checklists)
	}
	intent := patch.Checklist
	if intent == nil || intent.Op != tracker.ChecklistAddList || intent.List == "" ||
		len(intent.Items) != 2 || intent.Items[0].ID == "" ||
		intent.Items[0].ID == intent.Items[1].ID || intent.Items[1].Name != "announce" {
		t.Fatalf("the gesture is %+v, want add_list with a minted list id and "+
			"two minted items in order", intent)
	}
	if trk.kinds[0] != tracker.ChangeChecklist {
		t.Errorf("the change kind is %q, want checklist", trk.kinds[0])
	}

	// A RETRY NAMES THE SAME LIST: the ids derive from the operation, so a
	// repeated request cannot add the list twice.
	again := newFakeTracker()
	regAgain := workRegistry(t, builtin.WorkDeps{Reader: again, Writer: again.as})
	if got := callWork(t, regAgain, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "checklist": map[string]any{
			"op": "add_list", "name": "Launch", "items": []any{"migrate", "announce"},
		},
	}); got.Failed {
		t.Fatalf("the retried add_list failed: %s", got.Output)
	}
	if again.patched[0].Checklist.List != intent.List {
		t.Errorf("a retried add_list minted %q and then %q — a redelivered turn "+
			"would add the list twice", intent.List, again.patched[0].Checklist.List)
	}
}

// TWO DIFFERENT UPDATES IN ONE TURN ARE TWO OPERATIONS.
//
// The operation id was the seed and the task alone, so a turn that ticked two
// items — or set the status and then assigned the item — wrote its second
// change under the first one's id, and the broker collapsed it as a
// redelivery and answered `applied` for a change that never landed.
func TestTwoDifferentUpdatesInOneTurnAreTwoOperations(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	for _, item := range []string{"a", "b"} {
		if got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
			"item": "ENG-1", "checklist": map[string]any{
				"op": "set_done", "item": item, "done": true,
			},
		}); got.Failed {
			t.Fatalf("tick %s failed: %s", item, got.Output)
		}
	}
	if len(trk.opIDs) != 2 || trk.opIDs[0] == trk.opIDs[1] {
		t.Fatalf("two ticks in one turn wrote under %v — the second is "+
			"collapsed as a redelivery of the first", trk.opIDs)
	}
	// AND THE SAME CALL TWICE IS STILL ONE: a re-run turn re-issues it.
	if got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "checklist": map[string]any{
			"op": "set_done", "item": "a", "done": true,
		},
	}); got.Failed {
		t.Fatalf("the repeated tick failed: %s", got.Output)
	}
	if trk.opIDs[2] != trk.opIDs[0] {
		t.Errorf("the same tick wrote under %q and then %q — a re-run turn "+
			"would tick it as two operations", trk.opIDs[0], trk.opIDs[2])
	}
}

// A CHECKLIST GESTURE THAT CANNOT BE READ IS REFUSED BEFORE ANYTHING IS SENT,
// naming what it needed.
func TestAMalformedChecklistGestureIsRefused(t *testing.T) {
	t.Parallel()
	for name, spec := range map[string]any{
		"not an object":         []any{"a"},
		"an unknown op":         map[string]any{"op": "tick"},
		"the promotion":         map[string]any{"op": "promote", "item": "a"},
		"set_done with no done": map[string]any{"op": "set_done", "item": "a"},
		"assign with nobody":    map[string]any{"op": "assign_item", "item": "a"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
			got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
				"item": "ENG-1", "checklist": spec,
			})
			if !got.Failed {
				t.Fatalf("%s was accepted: %s", name, got.Output)
			}
			if len(trk.patched) != 0 {
				t.Errorf("a refused gesture still wrote %v", trk.patched)
			}
		})
	}
}
