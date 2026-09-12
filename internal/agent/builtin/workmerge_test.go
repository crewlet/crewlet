package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A MERGE RESOLVES BOTH ENDS AND MOVES THE CHILDREN BY DEFAULT.
//
// The fold is a sequence and the tool's half of it is the resolution: a model
// types the keys it read, and a sequence handed a key writes a parent pointer
// that resolves to nothing on every node. The default matters as much — a bool
// argument's zero value is "leave the subtasks under a cancelled parent",
// which is the orphan this verb exists to prevent.
func TestAMergeResolvesBothItemsAndMovesTheSubtasks(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Merges: trk.merges,
	})

	if got := callWork(t, reg, builtin.MergeWorkItemTool, map[string]any{
		"item": "ENG-1", "into": "eng-2",
	}); got.Failed {
		t.Fatalf("the merge failed: %s", got.Output)
	}
	if len(trk.merged) != 1 {
		t.Fatalf("the call made %d folds", len(trk.merged))
	}
	fold := trk.merged[0]
	if fold.duplicate != "i1" || fold.into != "id-2" {
		t.Errorf("the fold names %q into %q — both ends are ids, because a "+
			"key written into a parent pointer resolves to nothing on every "+
			"node for ever", fold.duplicate, fold.into)
	}
	if !fold.reparent {
		t.Error("the subtasks were left under the closed item although " +
			"nobody asked for that — an absent boolean is the safe outcome " +
			"here, not the zero one")
	}
	// AND THE WAKE DESCRIBES WHAT THE FOLD LEAVES. The close is the last
	// append, so a snapshot still showing the item open would be the last
	// word anybody hears about it.
	if fold.notify == nil {
		t.Fatal("the fold told nobody")
	}
	if got := fold.notify.Snapshot.Status; got != tracker.StatusCancelled {
		t.Errorf("the wake's snapshot says %q and the fold closes the item", got)
	}
}

// AND `move_subtasks: false` IS HONOURED, because it is the one case where
// leaving them is what somebody means.
func TestAMergeCanBeToldToLeaveTheSubtasks(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Merges: trk.merges,
	})

	if got := callWork(t, reg, builtin.MergeWorkItemTool, map[string]any{
		"item": "ENG-1", "into": "eng-2", "move_subtasks": false,
	}); got.Failed {
		t.Fatalf("the merge failed: %s", got.Output)
	}
	if trk.merged[0].reparent {
		t.Error("`move_subtasks: false` still moved them")
	}
}

// AN ITEM IS NOT FOLDED INTO ITSELF, and the refusal is the tool's rather than
// the sequence's: the two arguments may be a key and an id naming one item, so
// the check has to happen after both are resolved.
func TestAMergeRefusesToFoldAnItemIntoItself(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Merges: trk.merges,
	})

	got := callWork(t, reg, builtin.MergeWorkItemTool, map[string]any{
		"item": "i1", "into": "ENG-1",
	})
	if !got.Failed || !strings.Contains(got.Output, "itself") {
		t.Fatalf("folding an item into itself gave %q", got.Output)
	}
	if len(trk.merged) != 0 {
		t.Error("the sequence was called anyway")
	}
}

// A BUILD WITH NO MERGE SEQUENCE DOES NOT ADVERTISE THE VERB.
//
// [Register]'s own rule: the fold reads a subtree and takes a fleet claim, so
// a surface holding no replicated estate cannot serve it — and a catalogue
// offering a tool that always fails is how a model learns to distrust all of
// them. The dependency ARGUMENTS refuse by name instead, because they are
// facets of a verb that still works without them; a whole verb has no such
// remainder.
func TestNoMergeSequenceMeansNoMergeTool(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	if _, held := reg.Lookup(builtin.MergeWorkItemTool); held {
		t.Error("merge_work_item was registered with no merge sequence behind it")
	}
}
