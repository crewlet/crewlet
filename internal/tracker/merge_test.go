package tracker_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// FOLDING ONE ITEM INTO ANOTHER IS THREE COMMITS AND ALL OF THEM LAND.
//
// The sequence had no caller and no test at all, which is why the shape of its
// second append — the duplicate's own subject, written again after the mark —
// was never exercised. What it has to leave behind is one state: the duplicate
// cancelled and linked to the survivor, its children under the survivor, and
// nothing under a closed parent.
func TestAMergeClosesTheDuplicateAndMovesItsChildren(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE APPLIER RUNS, because a fold writes the duplicate's own subject
	// twice and its close waits for its mark. See applyWhileWriting.
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")

	parent := "dup"
	child := newTask("kid")
	child.Parent, child.Depth = &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", child, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
	}
	r.drain()
	under := "kid"
	grand := newTask("grandkid")
	grand.Parent, grand.Depth = &under, 2
	if _, err := r.writer.CreateTask(t.Context(), "op-grand", grand, nil); err != nil {
		t.Fatalf("CreateTask grandkid: %v", err)
	}
	r.drain()

	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		true, nil); err != nil {
		t.Fatalf("MergeDuplicates: %v", err)
	}
	r.drain()

	dup := r.task(t, "dup")
	if dup.Task.Status != tracker.StatusCancelled {
		t.Errorf("the duplicate is %q and a fold closes it", dup.Task.Status)
	}
	if !slices.ContainsFunc(dup.Task.Relations, func(rel tracker.Relation) bool {
		return rel.Kind == tracker.RelationDuplicates && rel.Other == "keep"
	}) {
		t.Errorf("the duplicate carries %+v and names nothing it duplicates — "+
			"a closed item with no link is one nobody can follow to the work",
			dup.Task.Relations)
	}
	// THE CHILD IS THE HALF NO PATCH CAN DO. A seat can cancel and link by
	// hand; it cannot move the subtree, and a subtask left under a closed
	// parent is work that disappears with it.
	if got := parentOf(r.task(t, "kid")); got != "keep" {
		t.Fatalf("the child's parent is %q, and the item it was under is "+
			"closed", got)
	}
	// AND ONLY THE SUBTASKS MOVE. A grandchild travels UNDER its own
	// parent, because that is what `move_subtasks` promises and what a
	// tree is: re-parenting every descendant onto the survivor flattens
	// the subtree, which nobody asked for and which no later gesture can
	// reconstruct.
	if got := parentOf(r.task(t, "grandkid")); got != "kid" {
		t.Errorf("the grandchild's parent is %q, want \"kid\" — the merge "+
			"flattened the subtree onto the survivor rather than moving the "+
			"subtasks it was asked to move", got)
	}
	// AND THE MERGE FLAG IS CLEARED. It is raised by the mark and lowered
	// by the close, so an item left `merging` is a fold that stopped
	// halfway — which is exactly what a reader must be able to tell from
	// one that finished.
	if dup.Task.Merging {
		t.Error("the duplicate is still marked merging after the fold closed it")
	}
	// AND SO IS THE INTENT IT CARRIED. It describes a walk that is
	// RUNNING, so "not merging, but re-parenting" is a state the duty
	// would read as an instruction with nothing to instruct — and the
	// close names only the marker, so the two go down together or they
	// drift on the first completed merge.
	if dup.Task.MergeReparent {
		t.Error("the finished merge left its re-parent intent standing on the " +
			"task, which no later marker on that task is the author of")
	}
}

// AND THE CHILDREN STAY WHERE THEY ARE WHEN NOBODY ASKED TO MOVE THEM.
func TestAMergeLeavesTheChildrenWhenItIsNotAskedToReparent(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE APPLIER RUNS, because a fold writes the duplicate's own subject
	// twice and its close waits for its mark. See applyWhileWriting.
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")

	parent := "dup"
	child := newTask("kid")
	child.Parent, child.Depth = &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", child, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
	}
	r.drain()
	under := "kid"
	grand := newTask("grandkid")
	grand.Parent, grand.Depth = &under, 2
	if _, err := r.writer.CreateTask(t.Context(), "op-grand", grand, nil); err != nil {
		t.Fatalf("CreateTask grandkid: %v", err)
	}
	r.drain()

	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		false, nil); err != nil {
		t.Fatalf("MergeDuplicates: %v", err)
	}
	r.drain()

	if got := parentOf(r.task(t, "kid")); got != "dup" {
		t.Errorf("the child moved to %q although the fold was told to leave it",
			got)
	}
}

// A REMOVED ITEM IS NOT FOLDED, and the refusal says what to do about it.
//
// The pre-flight is what makes that answerable: the duplicate is read before the
// first append, so a fold that cannot complete refuses whole rather than
// leaving the duplicate marked `merging` with nothing to finish it.
func TestAMergeRefusesARemovedItemAndSaysToRestoreIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE APPLIER RUNS, because a fold writes the duplicate's own subject
	// twice and its close waits for its mark. See applyWhileWriting.
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")

	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "dup", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	_, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep", true, nil)
	if err == nil {
		t.Fatal("a removed item was folded")
	}
	if !strings.Contains(err.Error(), "restore") {
		t.Errorf("the refusal is %q and does not name the move that would "+
			"make the fold possible", err)
	}
}

// parentOf is a task's parent, or "" where it has none.
func parentOf(detail tracker.TaskDetail) string {
	if detail.Task.Parent == nil {
		return ""
	}
	return *detail.Task.Parent
}

// hookedAppender runs a hook once, straight after the first append its match
// accepts — which is how a case puts a purge between two appends of one
// sequence, at the one point in it the case is about.
type hookedAppender struct {
	statelog.Appender

	mu    sync.Mutex
	match func(subject, opID string) bool
	hook  func()
	fired bool
}

// didFire reports whether the hook has run.
func (a *hookedAppender) didFire() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fired
}

// arm sets what the hook fires on, and what it does. Nothing fires before it
// is armed, so the appends a case makes to build its fixture pass untouched.
func (a *hookedAppender) arm(match func(subject, opID string) bool, hook func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.match, a.hook = match, hook
}

func (a *hookedAppender) Append(ctx context.Context, subject, msgID string,
	expect *uint64, body []byte) (uint64, bool, error) {

	seq, duplicate, err := a.Appender.Append(ctx, subject, msgID, expect, body)
	if err != nil {
		return seq, duplicate, err
	}
	a.mu.Lock()
	fire := !a.fired && a.match != nil && a.match(subject, msgID)
	if fire {
		a.fired = true
	}
	hook := a.hook
	a.mu.Unlock()
	if fire {
		hook()
	}
	return seq, duplicate, nil
}

// newHookedRoundTrip is a round trip whose appends pass through a hook.
func newHookedRoundTrip(t *testing.T) (*roundTrip, *hookedAppender) {
	t.Helper()
	hooked := &hookedAppender{}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		hooked.Appender = a
		return hooked
	})
	return r, hooked
}

// purgeNow purges a task and applies it on this node, which is what a purge
// published by a colleague looks like to the next snapshot taken here.
func purgeNow(t *testing.T, r *roundTrip, id string) {
	t.Helper()
	if _, err := r.writer.PurgeTask(t.Context(), "op-purge-"+id, id, "ENG",
		"filed twice"); err != nil {
		t.Fatalf("purge %s: %v", id, err)
	}
	r.drain()
}

// A MERGE WHOSE TARGET IS PURGED PART-WAY IS GIVEN UP BY THE NEXT STEP, NOT
// FINISHED.
//
// Only the duplicate is read before the first append. Every append after it
// reads the target in its own snapshot — the mark and the close as the merge's
// target, a re-parent as the parent it is about to write — so a purge that
// lands between two appends is seen by the next one. The merge is then given
// up: the duplicate left open with its marker cleared, and [tracker.ErrPurged]
// returned. Finished instead, it re-parented subtasks onto a task no row holds
// and cancelled the duplicate as merged into nothing.
func TestAMergeWhoseTargetIsPurgedPartWayIsGivenUp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// after is the append the purge lands straight after.
		after string
		// kidParent is where the subtask ends: still under the
		// duplicate when the purge landed before its move, or where the
		// purge put the target's children when it landed after.
		kidParent string
	}{
		{"after the mark, before the subtasks move", "op-merge.mark", "dup"},
		{"after the subtasks moved, before the close", "op-merge.c/kid", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, hooked := newHookedRoundTrip(t)
			r.applyWhileWriting()
			filedTask(t, r, "keep")
			filedTask(t, r, "dup")
			parent := "dup"
			kid := newTask("kid")
			kid.Parent, kid.Depth = &parent, 1
			if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
				t.Fatalf("CreateTask kid: %v", err)
			}
			r.drain()
			hooked.arm(func(_, opID string) bool { return opID == tc.after },
				func() { purgeNow(t, r, "keep") })

			_, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup",
				"keep", true, nil)
			if !errors.Is(err, tracker.ErrPurged) || !errors.Is(err, tracker.ErrNoTask) {
				t.Fatalf("a merge whose target was purged part-way answered %v, "+
					"want the purge named", err)
			}
			r.drain()
			if !hooked.didFire() {
				t.Fatal("the purge never landed, so this case is not the shape it names")
			}
			dup := r.task(t, "dup")
			if dup.Task.Merging || dup.Task.Status == tracker.StatusCancelled {
				t.Errorf("the duplicate reads merging=%v and status %q, want the "+
					"marker cleared and the task left open", dup.Task.Merging,
					dup.Task.Status)
			}
			if got := parentOf(r.task(t, "kid")); got != tc.kidParent {
				t.Errorf("the subtask's parent is %q, want %q", got, tc.kidParent)
			}
			if named := r.strings(`SELECT id FROM tracker_tasks
				WHERE parent_id = 'keep'`); len(named) != 0 {
				t.Errorf("%v hang from the purged target", named)
			}
		})
	}
}

// A MERGE INTO A TASK ALREADY PURGED WRITES NOTHING.
//
// The mark reads its target in the snapshot it is decided from, so a target
// the node has already seen purged refuses the merge at its first append —
// before the marker exists for anything to finish.
func TestAMergeIntoAPurgedTaskWritesNothing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	purgeNow(t, r, "keep")
	before := r.consumed

	_, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		true, nil)
	if !errors.Is(err, tracker.ErrPurged) {
		t.Fatalf("a merge into a purged task answered %v, want the purge named", err)
	}
	r.drain()
	if r.consumed != before {
		t.Errorf("the refused merge appended %d record(s)", r.consumed-before)
	}
	if r.task(t, "dup").Task.Merging {
		t.Error("the refused merge left the duplicate marked mid-merge")
	}
}

// NOTHING IS FILED UNDER, OR MOVED ONTO, A PURGED TASK.
//
// A parent is read in the snapshot the record is decided from, and a purged
// one is refused naming it — never written as a parent no row holds, and never
// quietly replaced by one the caller did not name. The create's refusal comes
// before its key is minted, so a create that can never land takes no number.
func TestNothingIsFiledUnderAPurgedTask(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "gone")
	filedTask(t, r, "kid")
	purgeNow(t, r, "gone")
	counter := func() string {
		return r.strings(`SELECT CAST(last AS TEXT) FROM tracker_counters
			WHERE project_key = 'ENG'`)[0]
	}
	minted := counter()

	gone := "gone"
	child := newTask("child")
	child.Parent, child.Depth = &gone, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-child", child, nil); !errors.Is(err, tracker.ErrPurged) {
		t.Errorf("a create under a purged task answered %v, want the purge named", err)
	}
	// APPLIED BEFORE IT IS READ, or a mint that did land would still read
	// as the old number here.
	r.drain()
	if got := counter(); got != minted {
		t.Errorf("the refused create took a key: the counter moved from %s to %s",
			minted, got)
	}
	if _, err := r.writer.UpdateTask(t.Context(), "op-move", "kid", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Parent: &gone},
		tracker.ChangeReparented, nil); !errors.Is(err, tracker.ErrPurged) {
		t.Errorf("a move onto a purged task answered %v, want the purge named", err)
	}
	r.drain()
	if got := parentOf(r.task(t, "kid")); got != "" {
		t.Errorf("the refused move left kid under %q", got)
	}
}

// A PURGE BETWEEN A CREATE'S KEY AND ITS TASK IS SEEN BY THE TASK'S APPEND.
//
// A create is two appends — the key mint on the project's counter, then the
// task on its own subject — and each reads the parent in its own snapshot. A
// purge landing between the two is invisible to the mint and refused by the
// task's append, so nothing is filed under the purged task; the number the mint
// took is the create's documented crash residue, a gap in the keys.
func TestAPurgeBetweenACreatesKeyAndItsTaskIsSeen(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	filedTask(t, r, "gone")
	hooked.arm(func(_, opID string) bool { return opID == "op-child.counter" },
		func() { purgeNow(t, r, "gone") })

	gone := "gone"
	child := newTask("child")
	child.Parent, child.Depth = &gone, 1
	_, err := r.writer.CreateTask(t.Context(), "op-child", child, nil)
	if !hooked.didFire() {
		t.Fatal("the purge never landed between the two appends, so this case " +
			"is not the shape it names")
	}
	if !errors.Is(err, tracker.ErrPurged) {
		t.Fatalf("a create whose parent was purged after its key was minted "+
			"answered %v, want the purge named", err)
	}
	r.drain()
	if got := r.strings(`SELECT id FROM tracker_tasks WHERE id = 'child'`); len(got) != 0 {
		t.Fatal("the child was filed under a purged task")
	}
}

// ONE MERGE OF AN ITEM RUNS AT A TIME, ON ONE NODE AS ACROSS TWO.
//
// The merge's lease is taken for the NODE, and a lease asked for again by the
// owner that holds it is renewed rather than refused — so on the lease alone,
// two seats on one node folding one duplicate into two different items were
// both let in and walked its subtasks at once, each moving what the other had
// not. The second is refused while the first walks, and the first finishes as
// if it were alone.
func TestOneMergeOfAnItemRunsAtATimeOnOneNode(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "other")
	filedTask(t, r, "dup")
	parent := "dup"
	kid := newTask("kid")
	kid.Parent, kid.Depth = &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
	}
	r.drain()
	// A SECOND SEAT ON THIS NODE, which is a clone of the one writer the
	// node built, asking while the first merge's mark has landed and its
	// walk has not.
	var second error
	hooked.arm(func(_, opID string) bool { return opID == "op-merge.mark" },
		func() {
			_, second = r.writer.As("bea", tracker.AuthorAgent, tracker.Provenance{}).
				MergeDuplicates(t.Context(), "op-other", "dup", "other", true, nil)
		})

	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		true, nil); err != nil {
		t.Fatalf("MergeDuplicates: %v", err)
	}
	if !hooked.didFire() {
		t.Fatal("the second merge was never attempted, so this case is not the " +
			"shape it names")
	}
	if !errors.Is(second, statelog.ErrUnavailable) {
		t.Fatalf("a second merge of the item while the first walked answered %v, "+
			"want it refused as already running", second)
	}
	r.drain()
	dup := r.task(t, "dup")
	if slices.ContainsFunc(dup.Task.Relations, func(rel tracker.Relation) bool {
		return rel.Other == "other"
	}) {
		t.Errorf("the duplicate is linked to the second merge's target: %+v",
			dup.Task.Relations)
	}
	if got := parentOf(r.task(t, "kid")); got != "keep" {
		t.Errorf("the subtask's parent is %q, want the first merge's target", got)
	}
	if dup.Task.Status != tracker.StatusCancelled || dup.Task.Merging {
		t.Errorf("the duplicate reads status %q and merging=%v after the first "+
			"merge finished", dup.Task.Status, dup.Task.Merging)
	}
}

// A MERGE REFUSED BECAUSE THE STORE COULD NOT ANSWER CAN RUN ONCE IT DOES.
//
// A walk fails closed on an unreachable coordination store — unknown is not
// "free", and two walks of one subtree interleaved are what the claim exists
// to prevent. It asks for this node's half of the claim before the lease, so
// that refusal has to hand the half back: kept, the next attempt would read on
// this node as a walk already running, for as long as the process lives.
func TestAMergeRefusedOnAnUnreachableStoreRunsOnceItAnswers(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")

	r.claims.Break(nil)
	before := r.consumed
	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		true, nil); err == nil {
		t.Fatal("a merge ran with the coordination store unreachable")
	}
	r.drain()
	if r.consumed != before {
		t.Fatalf("the refused merge appended %d record(s)", r.consumed-before)
	}

	r.claims.Heal()
	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge-again", "dup",
		"keep", true, nil); err != nil {
		t.Fatalf("the merge, once the store answered again: %v", err)
	}
	r.drain()
	if got := r.task(t, "dup").Task.Status; got != tracker.StatusCancelled {
		t.Errorf("the duplicate is %q after its merge", got)
	}
}

// A CLAIM IS NOT FREE ON THIS NODE UNTIL ITS LEASE HAS BEEN GIVEN BACK.
//
// The lease is the node's, so a goroutine here that asked for it while it was
// still held would be told yes — renewed, not refused — and walk under a lease
// the release in progress was about to give up. So a release gives the lease
// back first and this node's half last, and a walk asking in between is
// refused by the half still held.
func TestAClaimIsNotFreeHereUntilItsLeaseIsGivenBack(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "other")
	filedTask(t, r, "dup")
	claims := &releasingClaims{Claims: r.claims}
	writer, err := tracker.NewWriter(tracker.WriterDeps{
		Publisher: r.publisher, DB: r.db, NodeID: "node-a", Claims: claims,
		Actor: "ana", ActorKind: tracker.AuthorHuman,
		Now: func() time.Time { return r.at },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	var second error
	claims.arm(func() {
		_, second = writer.As("bea", tracker.AuthorAgent, tracker.Provenance{}).
			MergeDuplicates(t.Context(), "op-other", "dup", "other", false, nil)
	})

	if _, err := writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		false, nil); err != nil {
		t.Fatalf("MergeDuplicates: %v", err)
	}
	if !claims.fired() {
		t.Fatal("nothing asked for the claim while it was being given back, so " +
			"this case is not the shape it names")
	}
	if !errors.Is(second, statelog.ErrUnavailable) {
		t.Fatalf("a merge asking while the claim's lease was being given back "+
			"answered %v, want it refused as already running", second)
	}
}

// releasingClaims runs a hook once, as a lease is being given back and before
// the store has heard of it — the one moment a claim is half released.
type releasingClaims struct {
	tracker.Claims

	mu   sync.Mutex
	hook func()
	ran  bool
}

func (c *releasingClaims) arm(hook func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hook = hook
}

func (c *releasingClaims) fired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ran
}

func (c *releasingClaims) Release(ctx context.Context, resource, owner string,
	epoch int64) (bool, error) {

	c.mu.Lock()
	hook := c.hook
	c.hook, c.ran = nil, c.ran || hook != nil
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return c.Claims.Release(ctx, resource, owner, epoch)
}

// A SUBTASK MOVED ELSEWHERE WHILE A MERGE WALKS IS NOT MOVED BACK.
//
// The walk reads a batch of the duplicate's subtasks and then moves them one
// append at a time, so a subtask somebody re-parents in between is one the
// batch still names. Its move reads the subtask again in its own decide and
// passes over one that is no longer under the duplicate — decided from the
// batch instead, it moved it onto the target over the decision that had just
// taken it somewhere else.
func TestASubtaskMovedElsewhereDuringAMergeStaysMoved(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "elsewhere")
	filedTask(t, r, "dup")
	parent := "dup"
	for _, id := range []string{"kid-1", "kid-2"} {
		kid := newTask(id)
		kid.Parent, kid.Depth = &parent, 1
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, kid, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	elsewhere := "elsewhere"
	hooked.arm(func(_, opID string) bool { return opID == "op-merge.c/kid-1" },
		func() {
			if _, err := r.writer.As("bea", tracker.AuthorAgent, tracker.Provenance{}).
				UpdateTask(t.Context(), "op-move", "kid-2", "ENG", tracker.NoIfMatch,
					tracker.TaskPatch{Parent: &elsewhere}, tracker.ChangeReparented,
					nil); err != nil {
				t.Errorf("move kid-2: %v", err)
			}
			r.drain()
		})

	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		true, nil); err != nil {
		t.Fatalf("MergeDuplicates: %v", err)
	}
	if !hooked.didFire() {
		t.Fatal("nothing moved a subtask during the walk, so this case is not " +
			"the shape it names")
	}
	r.drain()
	if got := parentOf(r.task(t, "kid-2")); got != "elsewhere" {
		t.Errorf("the subtask moved during the merge is under %q, want where "+
			"it was moved to", got)
	}
	if got := parentOf(r.task(t, "kid-1")); got != "keep" {
		t.Errorf("the subtask the merge moved is under %q, want keep", got)
	}
	if got := r.task(t, "dup").Task.Status; got != tracker.StatusCancelled {
		t.Errorf("the duplicate is %q after its merge", got)
	}
}

// A SUBTASK IN THE TRASH STAYS WHERE IT IS, AND THE MERGE STILL FINISHES.
//
// A tombstone is a freeze: every patch on a removed task is refused. The walk
// reads a removed subtask like any other child, so moving it was refused on
// every attempt — a merge that could never close, and a sweep failing on it at
// every tick. It is passed over; restored later, it comes back where it was
// removed from.
func TestASubtaskInTheTrashDoesNotStopAMerge(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	parent := "dup"
	for _, id := range []string{"kid-1", "kid-2"} {
		kid := newTask(id)
		kid.Parent, kid.Depth = &parent, 1
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, kid, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "kid-2", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		true, nil); err != nil {
		t.Fatalf("a merge of an item with a subtask in the trash answered %v", err)
	}
	r.drain()
	if got := parentOf(r.task(t, "kid-1")); got != "keep" {
		t.Errorf("the live subtask is under %q, want keep", got)
	}
	trashed := r.task(t, "kid-2")
	if got := parentOf(trashed); got != "dup" || trashed.Task.Removed == nil {
		t.Errorf("the subtask in the trash is under %q with tombstone %v, want "+
			"it left in the trash under dup", got, trashed.Task.Removed)
	}
	dup := r.task(t, "dup")
	if dup.Task.Status != tracker.StatusCancelled || dup.Task.Merging {
		t.Errorf("the duplicate reads status %q and merging=%v", dup.Task.Status,
			dup.Task.Merging)
	}
}

// A MERGE ENDED BY ANOTHER WRITER WHILE IT WALKED IS NOT CLOSED OVER THAT.
//
// The close reads the merge as still running in its own decide. A marker some
// other write cleared after the mark means the merge was ended elsewhere, and
// closing it anyway would cancel an item on the strength of a read that no
// longer holds — so the close writes nothing and the call says the merge had
// already been ended.
func TestAMergeEndedByAnotherWriterIsNotClosedOverIt(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	done := false
	hooked.arm(func(_, opID string) bool { return opID == "op-merge.mark" },
		func() {
			r.drain()
			if _, err := r.writer.UpdateTask(t.Context(), "op-clear", "dup", "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{Merging: &done},
				tracker.ChangeFields, nil); err != nil {
				t.Errorf("clear the marker: %v", err)
			}
			r.drain()
		})

	_, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		false, nil)
	if !hooked.didFire() {
		t.Fatal("nothing ended the merge while it walked, so this case is not " +
			"the shape it names")
	}
	if err == nil || !strings.Contains(err.Error(), "ended by another writer") {
		t.Fatalf("a merge ended by another writer answered %v, want it to say so",
			err)
	}
	r.drain()
	if got := r.task(t, "dup").Task.Status; got == tracker.StatusCancelled {
		t.Error("the duplicate was cancelled by a merge somebody else had ended")
	}
}

// removeNow puts a task in the trash on its own — no subtree — and applies it
// on this node, which is what a removal published by a colleague looks like
// to the next snapshot taken here.
func removeNow(t *testing.T, r *roundTrip, id string) {
	t.Helper()
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove-"+id, id, "ENG",
		false, nil); err != nil {
		t.Fatalf("remove %s: %v", id, err)
	}
	r.drain()
}

// A MERGE INTO AN ITEM IN THE TRASH WRITES NOTHING, AND SAYS TO RESTORE IT.
//
// The mark reads its target in the snapshot it is decided from. A target in
// the trash is refused there — before the marker exists for anything to
// finish — because every subtask the merge moves would be placed under it,
// which is refused on every attempt, and a duplicate closed as merged into it
// would point at an item nobody can see. Admitted, the merge left its marker
// on the duplicate and a caller told the tracker duty would complete it.
//
// Mutation: drop the removed check from mergeTargetHeld and the mark lands.
func TestAMergeIntoAnItemInTheTrashWritesNothing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	parent := "dup"
	kid := newTask("kid")
	kid.Key, kid.Parent, kid.Depth = "ENG-kid", &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
	}
	r.drain()
	removeNow(t, r, "keep")
	before := r.consumed

	_, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		true, nil)
	if err == nil || !strings.Contains(err.Error(), "restore it before merging into it") {
		t.Fatalf("a merge into an item in the trash answered %v, want it refused "+
			"naming the restore", err)
	}
	r.drain()
	if r.consumed != before {
		t.Errorf("the refused merge appended %d record(s)", r.consumed-before)
	}
	if dup := r.task(t, "dup"); dup.Task.Merging || len(dup.Task.Relations) != 0 {
		t.Errorf("the refused merge left the duplicate merging=%v with relations "+
			"%+v", dup.Task.Merging, dup.Task.Relations)
	}
	if got := parentOf(r.task(t, "kid")); got != "dup" {
		t.Errorf("the subtask's parent is %q after a refused merge", got)
	}
}

// A MERGE WHOSE TARGET GOES IN THE TRASH PART-WAY IS GIVEN UP, NOT LEFT
// MARKED.
//
// Every step after the mark reads the target in its own snapshot, and a
// target in the trash refuses them all alike: the next subtask's move as a
// parent in the trash, and the close as a merge target in it. Neither clears
// with time, so the merge is given up as it is for a purged target — the
// marker cleared and the duplicate left open — and the answer names the
// restore. Retried instead, the merge kept its marker and every sweep of the
// tracker duty met the same refusal, stopping at it before any abandoned merge
// that sorted after it.
//
// Mutation: give up on a purged target alone and the duplicate is left marked
// mid-merge.
func TestAMergeWhoseTargetIsRemovedPartWayIsGivenUp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// after is the append the removal lands straight after.
		after string
		// kidParent is where the subtask ends: still under the
		// duplicate when the removal landed before its move, and under
		// the target — a removal moves nothing — when it landed after.
		kidParent string
	}{
		{"after the mark, before the subtasks move", "op-merge.mark", "dup"},
		{"after the subtasks moved, before the close", "op-merge.c/kid", "keep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, hooked := newHookedRoundTrip(t)
			r.applyWhileWriting()
			filedTask(t, r, "keep")
			filedTask(t, r, "dup")
			parent := "dup"
			kid := newTask("kid")
			kid.Key, kid.Parent, kid.Depth = "ENG-kid", &parent, 1
			if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
				t.Fatalf("CreateTask kid: %v", err)
			}
			r.drain()
			hooked.arm(func(_, opID string) bool { return opID == tc.after },
				func() { removeNow(t, r, "keep") })

			_, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup",
				"keep", true, nil)
			if !hooked.didFire() {
				t.Fatal("the removal never landed, so this case is not the shape " +
					"it names")
			}
			var stopped *tracker.PartialError
			if err == nil || errors.As(err, &stopped) ||
				!strings.Contains(err.Error(), "given up") ||
				!strings.Contains(err.Error(), "restore") {
				t.Fatalf("a merge whose target went in the trash part-way "+
					"answered %v, want it given up naming the restore", err)
			}
			r.drain()
			dup := r.task(t, "dup")
			if dup.Task.Merging || dup.Task.Status == tracker.StatusCancelled {
				t.Errorf("the duplicate reads merging=%v and status %q, want the "+
					"marker cleared and the task left open", dup.Task.Merging,
					dup.Task.Status)
			}
			if got := parentOf(r.task(t, "kid")); got != tc.kidParent {
				t.Errorf("the subtask's parent is %q, want %q", got, tc.kidParent)
			}
		})
	}
}
