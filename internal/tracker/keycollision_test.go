package tracker_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A KEY TWO TASKS HOLD IS FLAGGED ON THE ONES THAT DID NOT CLAIM IT, and the
// key goes on opening the one that did.
//
// The broker cannot refuse the shape: a counter and a task are different
// subjects, so a counter restored beside tasks minted after it hands out
// numbers those tasks already hold, and each create arbitrates only against
// itself. So the applier writes every task, and the attention set is the only
// place anybody learns that one key names several — which it did not, because
// the flag's only producer was a cross-project move nothing called.
//
// The CLAIMANT keeps the key because it is the task every earlier reference,
// link and chat message was written against. Resolving the key to whichever
// row the key index returns first is a different answer wherever the row
// order differs from the claim order, which is exactly what a purge that
// hands the key on produces here — and what a donated snapshot's copy does to
// row order in general. A purge of the claimant HANDS THE KEY ON, or the
// survivors stay flagged, and the key unclaimed, until their own next write.
func TestAKeyTwoTasksHoldIsFlaggedOnTheOnesThatDidNotClaimIt(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	// CREATED OUT OF ID ORDER, so the task a purge hands the key to (the
	// lowest id) is not the one the key index lists first (the earliest
	// row).
	for _, id := range []string{"t-1", "t-3", "t-2"} {
		task := newTask(id)
		task.Key = "ENG-7"
		if _, err := h.apply(taskRecord(id, tracker.OpCreate, task, nil), at); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	flag := func(id string) int64 {
		t.Helper()
		return h.value(`SELECT key_collision FROM tracker_tasks WHERE id = ?`, id)
	}
	if flag("t-1") != 0 {
		t.Error("the task that claimed ENG-7 first is flagged — the claimant " +
			"is the one every earlier reference names, not a duplicate")
	}
	for _, id := range []string{"t-2", "t-3"} {
		if flag(id) != 1 {
			t.Errorf("%s holds ENG-7 beside its claimant and carries no "+
				"key_collision — the attention set is the only place a "+
				"duplicate key is ever named", id)
		}
	}

	log, err := statelogtest.LocalReader(tracker.Domain{}, h.db.Replicated(),
		statelog.Position{Stream: tracker.Domain{}.Stream().Name})
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := tracker.NewReader(h.db, log)
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	opened := func() string {
		t.Helper()
		got, err := reader.Task(t.Context(), "eng-7", tracker.DetailWants{},
			statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			t.Fatalf("read ENG-7: %v", err)
		}
		return got.Task.ID
	}
	if got := opened(); got != "t-1" {
		t.Errorf("ENG-7 opens %s, want t-1 — the claimant", got)
	}

	// A LATER WRITE TO A DUPLICATE KEEPS ITS FLAG: the flag is derived from
	// the directory on every apply, not set once by the create.
	patch := taskRecord("t-3", tracker.OpPatch,
		tracker.TaskPatch{Title: ptr("still a duplicate")}, nil)
	patch.OpID = "t-3-rename"
	if _, err := h.apply(patch, at.Add(time.Minute)); err != nil {
		t.Fatalf("patch t-3: %v", err)
	}
	if flag("t-3") != 1 {
		t.Error("an edit to a duplicate cleared its key_collision, although " +
			"the claimant still holds ENG-7")
	}

	purge := taskRecord("t-1", tracker.OpPurge,
		map[string]any{"reason": "the restored duplicate"}, nil)
	if _, err := h.apply(purge, at.Add(2*time.Minute)); err != nil {
		t.Fatalf("purge t-1: %v", err)
	}
	if got := h.value(`SELECT COUNT(*) FROM tracker_task_keys
		WHERE key = 'ENG-7' AND task_id = 't-2'`); got != 1 {
		t.Error("the directory does not name t-2 for ENG-7 after the " +
			"claimant's purge — the lowest id among the holders takes it, so " +
			"every node picks the same one")
	}
	if flag("t-2") != 0 {
		t.Error("t-2 claimed ENG-7 when the claimant was purged and is still " +
			"flagged — the purge did not re-derive the holders' flags")
	}
	if flag("t-3") != 1 {
		t.Error("t-3 still shares ENG-7 with t-2 and lost its key_collision")
	}
	if got := opened(); got != "t-2" {
		t.Errorf("ENG-7 opens %s after the purge, want t-2 — the new claimant, "+
			"whatever row the key index happens to list first", got)
	}
}
