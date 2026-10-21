package worklet_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// launchList is a task's one checklist with two lines.
func launchList() []worklet.Checklist {
	return []worklet.Checklist{{ID: "launch", Name: "Launch", Items: []worklet.ChecklistItem{
		{ID: "migrate", Name: "Migrate the users"},
		{ID: "flag", Name: "Flip the flag"},
	}}}
}

// checklistGesture is one gesture through the ordinary update path.
func checklistGesture(t *testing.T, r *roundTrip, opID, id string,
	gesture worklet.ChecklistIntent) error {

	t.Helper()
	_, err := r.writer.UpdateTask(t.Context(), opID, id, "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Checklist: &gesture}, worklet.ChangeChecklist, nil)
	r.drain()
	return err
}

// TICKING ONE ITEM KEEPS A LINE SOMEBODY ELSE ADDED.
//
// The collection is carried whole, so a tick formed from a caller's own read
// of the checklist — which is the only way a caller can form one — lands
// second and discards whatever arrived between that read and the write. The
// gesture is resolved against the task as the decide snapshot holds it, so the
// tick and the addition both survive in either order.
func TestTickingOneChecklistItemKeepsAConcurrentAdd(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-list")
	task.Checklists = launchList()
	if _, err := r.writer.CreateTask(t.Context(), "op-create", task, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	// The ticker decides from what it read: two lines, neither done.
	read := oneTask(t, r, "t-list")
	tick := worklet.ChecklistIntent{Op: worklet.ChecklistSetDone, Item: "migrate", Done: true}

	// A PEER ADDS A LINE between that read and the tick landing.
	peer := r.writer.As("ops", worklet.AuthorAgent, worklet.Provenance{TurnID: "turn-peer"})
	if _, err := peer.UpdateTask(t.Context(), "op-add", "t-list", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Checklist: &worklet.ChecklistIntent{
			Op: worklet.ChecklistAddItem, List: "launch", Item: "announce",
			Name: "Announce the cut-over",
		}}, worklet.ChangeChecklist, nil); err != nil {
		t.Fatalf("the peer's add: %v", err)
	}
	r.drain()

	if err := checklistGesture(t, r, "op-tick", "t-list", tick); err != nil {
		t.Fatalf("the tick: %v", err)
	}
	items := oneTask(t, r, "t-list").Checklists[0].Items
	if len(items) != 3 || items[2].ID != "announce" {
		t.Fatalf("after the tick the list is %+v — the line the peer added "+
			"after the ticker read %d lines is gone", items, len(read.Checklists[0].Items))
	}
	if !items[0].Done || items[1].Done || items[2].Done {
		t.Errorf("the tick left %+v — want exactly `migrate` done", items)
	}
}

// A GESTURE THAT GROWS THE CHECKLISTS PAST A CAP IS REFUSED — and one that
// shrinks them never is, or a task that somehow grew past a cap could never be
// brought back under it.
func TestAChecklistGesturePastTheCapIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	full := newTask("t-full")
	items := make([]worklet.ChecklistItem, 0, worklet.MaxChecklistItems)
	for i := range worklet.MaxChecklistItems {
		items = append(items, worklet.ChecklistItem{ID: "i" + itoa(i), Name: "line " + itoa(i)})
	}
	full.Checklists = []worklet.Checklist{{ID: "big", Name: "Big", Items: items}}
	if _, err := r.writer.CreateTask(t.Context(), "op-create", full, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	err := checklistGesture(t, r, "op-over", "t-full", worklet.ChecklistIntent{
		Op: worklet.ChecklistAddItem, List: "big", Item: "one-more", Name: "one more",
	})
	if !errors.Is(err, worklet.ErrInvalid) || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("a sixty-fifth line was %v, want a refusal naming the cap", err)
	}
	if got := len(oneTask(t, r, "t-full").Checklists[0].Items); got != worklet.MaxChecklistItems {
		t.Errorf("the refused line landed anyway: the list holds %d", got)
	}

	// SHRINKING AT THE CAP IS ALWAYS ALLOWED, and so is an in-place edit.
	if err := checklistGesture(t, r, "op-tick", "t-full", worklet.ChecklistIntent{
		Op: worklet.ChecklistSetDone, Item: "i0", Done: true,
	}); err != nil {
		t.Fatalf("a tick on a full list was refused: %v", err)
	}
	if err := checklistGesture(t, r, "op-drop", "t-full", worklet.ChecklistIntent{
		Op: worklet.ChecklistRemoveItem, Item: "i1",
	}); err != nil {
		t.Fatalf("a removal on a full list was refused: %v", err)
	}
	if got := len(oneTask(t, r, "t-full").Checklists[0].Items); got != worklet.MaxChecklistItems-1 {
		t.Errorf("after one removal the list holds %d", got)
	}
}

// THE GESTURES OVER VALUES, each one the rule it states.
func TestApplyChecklist(t *testing.T) {
	t.Parallel()
	parent := "migrate"
	nested := []worklet.Checklist{{ID: "launch", Name: "Launch", Items: []worklet.ChecklistItem{
		{ID: "migrate", Name: "Migrate"},
		{ID: "batch-1", Name: "Batch one", Parent: &parent},
		{ID: "flag", Name: "Flip the flag"},
	}}}
	manyLists := make([]worklet.Checklist, 0, worklet.MaxChecklists)
	for i := range worklet.MaxChecklists {
		manyLists = append(manyLists, worklet.Checklist{ID: "l" + itoa(i), Name: "list"})
	}

	for name, c := range map[string]struct {
		lists  []worklet.Checklist
		intent worklet.ChecklistIntent
		check  func(t *testing.T, got []worklet.Checklist)
		refuse string
		// mark is the refusal's class mark, ErrInvalid unless the case
		// says otherwise: an id another list or line holds is a
		// collision, ErrTaken.
		mark error
	}{
		"add a list with its first lines": {
			lists: launchList(),
			intent: worklet.ChecklistIntent{Op: worklet.ChecklistAddList, List: "qa", Name: " QA ",
				Items: []worklet.ChecklistItem{{ID: "smoke", Name: "Smoke test", Done: true}}},
			check: func(t *testing.T, got []worklet.Checklist) {
				if len(got) != 2 || got[1].Name != "QA" || len(got[1].Items) != 1 ||
					got[1].Items[0].Done {
					t.Errorf("got %+v — want a trimmed QA list whose new line is not done", got)
				}
			},
		},
		"remove an item and what is nested under it": {
			lists:  nested,
			intent: worklet.ChecklistIntent{Op: worklet.ChecklistRemoveItem, Item: "migrate"},
			check: func(t *testing.T, got []worklet.Checklist) {
				if len(got[0].Items) != 1 || got[0].Items[0].ID != "flag" {
					t.Errorf("got %+v — a child left behind points at nothing", got[0].Items)
				}
			},
		},
		"rename an item": {
			lists:  launchList(),
			intent: worklet.ChecklistIntent{Op: worklet.ChecklistRenameItem, Item: "flag", Name: "Flip it"},
			check: func(t *testing.T, got []worklet.Checklist) {
				if got[0].Items[1].Name != "Flip it" {
					t.Errorf("got %+v", got[0].Items)
				}
			},
		},
		"assign an item and give it back": {
			lists:  launchList(),
			intent: worklet.ChecklistIntent{Op: worklet.ChecklistAssignItem, Item: "flag", Assignee: "ops"},
			check: func(t *testing.T, got []worklet.Checklist) {
				if got[0].Items[1].Assignee != "ops" {
					t.Errorf("got %+v", got[0].Items)
				}
			},
		},
		"remove a list": {
			lists:  launchList(),
			intent: worklet.ChecklistIntent{Op: worklet.ChecklistRemoveList, List: "launch"},
			check: func(t *testing.T, got []worklet.Checklist) {
				if len(got) != 0 {
					t.Errorf("got %+v", got)
				}
			},
		},
		"an item id already taken": {
			lists:  launchList(),
			intent: worklet.ChecklistIntent{Op: worklet.ChecklistAddItem, List: "launch", Item: "flag", Name: "dup"},
			refuse: "already has a checklist item",
			mark:   worklet.ErrTaken,
		},
		"a list id already taken": {
			lists:  launchList(),
			intent: worklet.ChecklistIntent{Op: worklet.ChecklistAddList, List: "launch", Name: "dup"},
			refuse: "already has a checklist",
			mark:   worklet.ErrTaken,
		},
		"an unknown item": {
			lists:  launchList(),
			intent: worklet.ChecklistIntent{Op: worklet.ChecklistSetDone, Item: "nope", Done: true},
			refuse: "no checklist item",
		},
		"an empty name": {
			lists:  launchList(),
			intent: worklet.ChecklistIntent{Op: worklet.ChecklistRenameList, List: "launch", Name: "  "},
			refuse: "empty",
		},
		"a line longer than a line": {
			lists: launchList(),
			intent: worklet.ChecklistIntent{Op: worklet.ChecklistAddItem, List: "launch", Item: "x",
				Name: strings.Repeat("n", worklet.MaxChecklistItemName+1)},
			refuse: "maximum",
		},
		"a seventeenth list": {
			lists:  manyLists,
			intent: worklet.ChecklistIntent{Op: worklet.ChecklistAddList, List: "one-more", Name: "more"},
			refuse: "checklists",
		},
		"an unknown op": {
			lists:  launchList(),
			intent: worklet.ChecklistIntent{Op: "tick", Item: "flag"},
			refuse: "not a checklist gesture",
		},
		// A PROMOTION IS NO GESTURE AT ALL: an item becomes a task by
		// filing one, and nothing marks a line as having become it.
		"a promotion": {
			lists:  launchList(),
			intent: worklet.ChecklistIntent{Op: "promote", Item: "flag"},
			refuse: "not a checklist gesture",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			before := len(c.lists)
			got, err := worklet.ApplyChecklist(c.lists, c.intent)
			if c.refuse != "" {
				mark := c.mark
				if mark == nil {
					mark = worklet.ErrInvalid
				}
				if !errors.Is(err, mark) || !strings.Contains(err.Error(), c.refuse) {
					t.Fatalf("got %v, want a refusal marked %v mentioning %q", err, mark,
						c.refuse)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			c.check(t, got)
			if len(c.lists) != before {
				t.Errorf("the input was written through: %+v", c.lists)
			}
		})
	}
}

// AND THE INPUT IS NEVER WRITTEN THROUGH, because the decide runs again on a
// retry and the caller's snapshot is the tool's read.
func TestApplyChecklistLeavesItsInputAlone(t *testing.T) {
	t.Parallel()
	lists := launchList()
	if _, err := worklet.ApplyChecklist(lists, worklet.ChecklistIntent{
		Op: worklet.ChecklistSetDone, Item: "migrate", Done: true,
	}); err != nil {
		t.Fatal(err)
	}
	if lists[0].Items[0].Done {
		t.Error("ApplyChecklist ticked the caller's own copy")
	}
}

// A GESTURE BESIDE A WHOLE COLLECTION IS A PROGRAMMING ERROR, refused rather
// than resolved in some order.
func TestAChecklistGestureBesideACollectionIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-both")
	task.Checklists = launchList()
	if _, err := r.writer.CreateTask(t.Context(), "op-create", task, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	whole := launchList()
	_, err := r.writer.UpdateTask(t.Context(), "op-both", "t-both", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{
			Checklist:  &worklet.ChecklistIntent{Op: worklet.ChecklistSetDone, Item: "flag", Done: true},
			Checklists: &whole,
		}, worklet.ChangeChecklist, nil)
	if !errors.Is(err, worklet.ErrInvalid) {
		t.Fatalf("a patch carrying both was %v — one of them was silently discarded", err)
	}
}

// THE TASK ANSWER SERVES THE HAND-OFF BUDGET beside the counter it bounds, so
// no screen carries a figure of its own.
func TestTheTaskAnswerServesTheHandOffBudget(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("the budgeted one")
	detail, err := r.reader.Task(t.Context(), task.ID, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if detail.ReassignmentBudget != worklet.ReassignmentBudget || detail.ReassignmentBudget == 0 {
		t.Errorf("the answer serves a budget of %d, want %d", detail.ReassignmentBudget,
			worklet.ReassignmentBudget)
	}
}
