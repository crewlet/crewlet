package worklet_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE DUTY FINISHES ONLY A MERGE WHOSE HOLDER HAS LET GO.
//
// A task is marked mid-merge for the whole of a live merge's walk, so the
// marker cannot tell a walk that died from one still running — and the duty
// used to finish every marked task it found, racing a live walk child by child
// and publishing a second close behind the walk's own. The claim is what
// tells the two apart: a live walk heartbeats it, and a dead one's lapses.
//
// The claim is named here the way the tracker names it, as the `merge` class
// over the duplicate's id; were the tracker to rename it this case would see
// the duty finish a merge another node holds and go red rather than pass.
func TestTheDutyLeavesAMergeWhoseHolderIsAlive(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"keep", "dup"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id,
			newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	parent := "dup"
	kid := newTask("kid")
	kid.Parent = &parent
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
	}
	r.drain()

	// THE MARK A LIVE WALK HAS PUBLISHED, and the claim it still holds —
	// on another node, heartbeating.
	merging, reparent := true, true
	if _, err := r.writer.UpdateTask(t.Context(), "op-mark", "dup", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{
			Relate: &worklet.RelationIntent{Add: []worklet.Relation{{
				Kind: worklet.RelationDuplicates, Other: "keep",
			}}},
			Merging: &merging, MergeReparent: &reparent,
		}, worklet.ChangeRelations, nil); err != nil {
		t.Fatalf("UpdateTask mark: %v", err)
	}
	r.drain()
	resource := coord.Class("merge").Resource("dup")
	lease, refused, err := r.claims.TryAcquire(t.Context(), resource, coord.AcquireOptions{
		Owner: "node-b", TTL: worklet.ClaimTTL,
	})
	if err != nil || lease == nil {
		t.Fatalf("hold the walk's claim as node-b: %v, %q, %v", lease, refused, err)
	}

	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if swept["tracker_abandoned_merges"] != 0 {
		t.Errorf("the duty finished %d merge(s) while node-b's walk held the "+
			"claim — it raced a live walk", swept["tracker_abandoned_merges"])
	}
	r.drain()
	if got := r.task(t, "dup"); got.Task.Status == worklet.StatusCancelled ||
		!got.Task.Merging {
		t.Error("the duty closed a merge a live walk is still running")
	}
	if got := parentOf(r.task(t, "kid")); got != "dup" {
		t.Errorf("the duty moved the subtask to %q under a live walk", got)
	}

	// THE WALK DIES: its claim is given up (or lapses, which is the same
	// answer a TTL later), and the next tick finishes it.
	if _, err := r.claims.Release(t.Context(), resource, "node-b",
		lease.Epoch); err != nil {
		t.Fatalf("release node-b's claim: %v", err)
	}
	swept, err = trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if swept["tracker_abandoned_merges"] != 1 {
		t.Fatalf("the duty finished %d merge(s) once the claim was free, want 1",
			swept["tracker_abandoned_merges"])
	}
	r.drain()
	if got := r.task(t, "dup").Task; got.Status != worklet.StatusCancelled || got.Merging {
		t.Errorf("the abandoned merge is %q, merging=%v after the duty", got.Status, got.Merging)
	}
	if got := parentOf(r.task(t, "kid")); got != "keep" {
		t.Errorf("the subtask's parent is %q after the duty finished the merge", got)
	}
}
