package worklet_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN ITEM'S OWN DETAIL NAMES THE TASKS ITS HISTORY POINTS AT, so the
// dashboard renders "Parent: — → ENG-1" rather than the uuid. The detail read
// hands its history rows' sides to the walk the feed uses (held by
// `TestAReparentRecordsBothParentsByID`), so this asserts only that the detail
// read ASKS, from the raw snapshot shape its history keeps.
//
// The map depends on `History`: a read without it gets no map, omitted
// rather than a resolution of nothing.
func TestATaskDetailResolvesTheParentItsHistoryNames(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "t-parent")
	filedTask(t, r, "t-child")

	parent := "t-parent"
	if _, err := r.writer.UpdateTask(t.Context(), "op-adopt", "t-child", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Parent: &parent},
		worklet.ChangeReparented, nil); err != nil {
		t.Fatalf("re-parent: %v", err)
	}
	r.drain()

	// READ OFF THE ROW: the create mints the key from the project's
	// counter, not from the fixture's name.
	want := r.task(t, "t-parent").Task.Key
	if want == "" {
		t.Fatal("the parent has no key, so this case asserts nothing")
	}

	detail, err := r.reader.Task(t.Context(), "t-child",
		worklet.DetailWants{History: true},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the child: %v", err)
	}
	if got := detail.Keys["t-parent"]; got != want {
		t.Errorf("the detail resolves the new parent to %q, want %q — the "+
			"item's History tab renders the uuid without it", got, want)
	}
	// AN ID THIS NODE HOLDS NO ROW FOR IS ABSENT rather than empty, which
	// is what lets a renderer fall back to the id.
	if _, resolved := detail.Keys["t-nobody"]; resolved {
		t.Error("an id nothing was stored for came back resolved")
	}

	// AND WITHOUT THE HISTORY THERE IS NOTHING TO LABEL.
	bare, err := r.reader.Task(t.Context(), "t-child", worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the child without its history: %v", err)
	}
	if bare.Keys != nil {
		t.Errorf("a detail read with no history carries %d keys", len(bare.Keys))
	}
}
