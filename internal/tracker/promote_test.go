package tracker_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// promotionParent files a task carrying one checklist with the items named,
// and returns it.
func promotionParent(t *testing.T, r *roundTrip, id string, items ...string) {
	t.Helper()
	parent := newTask(id)
	list := tracker.Checklist{ID: "l-1", Name: "steps"}
	for _, item := range items {
		list.Items = append(list.Items, tracker.ChecklistItem{ID: item, Name: "do " + item})
	}
	parent.Checklists = []tracker.Checklist{list}
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, parent, nil); err != nil {
		t.Fatalf("file the parent: %v", err)
	}
	r.drain()
}

// promotedTo is where one of a parent's items points, or "" for nowhere.
func promotedTo(t *testing.T, r *roundTrip, parent, item string) string {
	t.Helper()
	for _, list := range taskOf(t, r, parent).Checklists {
		for _, it := range list.Items {
			if it.ID == item {
				if it.PromotedTo == nil {
					return ""
				}
				return *it.PromotedTo
			}
		}
	}
	t.Fatalf("task %s has no item %s", parent, item)
	return ""
}

// A RETRY OF A PROMOTION THAT FINISHED IS ANSWERED WITH IT: applied, at the
// parent's mark, with nothing appended.
//
// Its counter step is in the ledger, so the mint is answered rather than
// decided and cannot be built on. The promotion used to stop there, with an
// error saying the key it took "cannot be built on" — for a promotion that had
// done everything it was asked to.
func TestARetryOfAFinishedPromotionIsAnsweredWithIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	promotionParent(t, r, "parent-p", "item-1")
	op := statelog.NewOpID(time.Now(), "promote")

	first, err := r.writer.PromoteItem(t.Context(), op, "parent-p", "item-1",
		newTask("sub-s"), nil)
	if err != nil {
		t.Fatalf("the promotion: %v", err)
	}
	r.drain()
	end := r.logEnd(t)

	retry, err := r.writer.PromoteItem(t.Context(), op, "parent-p", "item-1",
		newTask("sub-s"), nil)
	if err != nil {
		t.Fatalf("the retry of a promotion that finished: %v", err)
	}
	if retry.Outcome != statelog.OutcomeApplied || !retry.Collapsed ||
		retry.Position != first.Position || retry.Key != first.Key {
		t.Fatalf("the retry = %+v key %q, want applied at the first run's %s as %s",
			retry.Result, retry.Key, first.Position, first.Key)
	}
	if got := r.logEnd(t); got != end {
		t.Fatalf("the retry put %d record(s) on the log", got-end)
	}
}

// A RETRY AFTER THE SUBTASK LANDED AND THE PARENT'S MARK DID NOT FINISHES IT —
// with no second subtask and no second key.
func TestARetryOfAHalfFinishedPromotionMarksTheParent(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	promotionParent(t, r, "parent-p", "item-1")
	lossy, log := r.lossyWriter(t)
	op := statelog.NewOpID(time.Now(), "promote")

	// THE PARENT'S APPEND IS REFUSED: the counter and the subtask land,
	// and the mark does not.
	log.refuse("parent-p")
	if _, err := lossy.PromoteItem(t.Context(), op, "parent-p", "item-1",
		newTask("sub-s"), nil); err == nil {
		t.Fatal("a promotion whose parent step was refused reported success")
	}
	log.refuse("")
	r.drain()
	filed := taskOf(t, r, "sub-s")
	if got := promotedTo(t, r, "parent-p", "item-1"); got != "" {
		t.Fatalf("the premise: the item already points at %q", got)
	}
	end := r.logEnd(t)

	retry, err := lossy.PromoteItem(t.Context(), op, "parent-p", "item-1",
		newTask("sub-s"), nil)
	if err != nil {
		t.Fatalf("the retry: %v — the subtask is filed and only the mark is "+
			"left, which a retry is told to finish", err)
	}
	r.drain()
	if retry.Outcome != statelog.OutcomeApplied || retry.Key != filed.Key {
		t.Errorf("the retry = %+v key %q, want applied as the subtask's own %s",
			retry.Result, retry.Key, filed.Key)
	}
	if got := promotedTo(t, r, "parent-p", "item-1"); got != "sub-s" {
		t.Errorf("after the retry the item points at %q, want sub-s", got)
	}
	if got := r.logEnd(t); got != end+1 {
		t.Errorf("the retry put %d record(s) on the log, want the parent's one "+
			"mark — no second counter record and no second subtask", got-end)
	}
}

// A PROMOTION KEEPS A CHECKLIST EDIT THAT LANDED WHILE IT RAN.
//
// The parent's lists are carried whole, and the mark was composed from the
// parent the mint read — before the subtask was filed — so an item somebody
// added to the parent in between was written away by the mark.
func TestAPromotionKeepsAChecklistEditThatLandedWhileItRan(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	promotionParent(t, r, "parent-p", "item-1")
	lossy, log := r.lossyWriter(t)

	// SOMEBODY ADDS AN ITEM the moment the subtask lands — after the
	// mint's read, before the mark.
	log.afterAppendTo("sub-s", func() {
		current := taskOf(t, r, "parent-p")
		lists := append([]tracker.Checklist{}, current.Checklists...)
		lists[0].Items = append(append([]tracker.ChecklistItem{}, lists[0].Items...),
			tracker.ChecklistItem{ID: "item-2", Name: "added meanwhile"})
		if _, err := r.writer.UpdateTask(t.Context(), "op-edit", "parent-p", "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Checklists: &lists},
			tracker.ChangeChecklist, nil); err != nil {
			t.Errorf("the concurrent edit: %v", err)
		}
	})
	if _, err := lossy.PromoteItem(t.Context(), statelog.NewOpID(time.Now(), "promote"),
		"parent-p", "item-1", newTask("sub-s"), nil); err != nil {
		t.Fatalf("the promotion: %v", err)
	}
	r.drain()

	items := taskOf(t, r, "parent-p").Checklists[0].Items
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	if len(items) != 2 {
		t.Fatalf("the parent's items are %v, want item-1 and the item-2 added "+
			"while the promotion ran — the mark wrote the list it read before", ids)
	}
	if got := promotedTo(t, r, "parent-p", "item-1"); got != "sub-s" {
		t.Errorf("item-1 points at %q, want sub-s", got)
	}
}

// THE MARK'S OTHER OUTCOMES: an item deleted meanwhile is told, not refused,
// and an item that already became another task is refused.
func TestAPromotionMarkSettlesAgainstTheParentItReads(t *testing.T) {
	t.Parallel()

	t.Run("an item deleted meanwhile is a warning and no record", func(t *testing.T) {
		t.Parallel()
		r := newRoundTrip(t)
		r.applyWhileWriting()
		promotionParent(t, r, "parent-p", "item-1", "item-2")
		end := r.logEnd(t)
		got, err := r.writer.UpdateTask(t.Context(), "op-mark", "parent-p", "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Promote: &tracker.PromoteIntent{
				Item: "item-gone", Subtask: "sub-s",
			}}, tracker.ChangeChecklist, nil)
		if err != nil {
			t.Fatalf("a mark of an item no longer there: %v — refusing it would "+
				"make the promotion unfinishable for a reason nothing undoes", err)
		}
		if got.Outcome != statelog.OutcomeApplied || len(got.Warnings) == 0 ||
			!strings.Contains(got.Warnings[0], "item-gone") {
			t.Errorf("the mark = %+v warnings %v, want applied with a warning "+
				"naming the item", got.Result, got.Warnings)
		}
		if n := r.logEnd(t); n != end {
			t.Errorf("a mark with nothing to mark put %d record(s) on the log", n-end)
		}
	})

	t.Run("an item that already became another task is refused", func(t *testing.T) {
		t.Parallel()
		r := newRoundTrip(t)
		r.applyWhileWriting()
		promotionParent(t, r, "parent-p", "item-1")
		if _, err := r.writer.PromoteItem(t.Context(), "op-promote", "parent-p",
			"item-1", newTask("sub-s"), nil); err != nil {
			t.Fatalf("the promotion: %v", err)
		}
		r.drain()
		_, err := r.writer.UpdateTask(t.Context(), "op-mark", "parent-p", "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Promote: &tracker.PromoteIntent{
				Item: "item-1", Subtask: "sub-other",
			}}, tracker.ChangeChecklist, nil)
		if err == nil || !strings.Contains(err.Error(), "already became task sub-s") {
			t.Errorf("a second promotion of one item = %v, want a refusal naming "+
				"the task it already became", err)
		}
	})
}
