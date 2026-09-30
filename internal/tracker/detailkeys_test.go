package tracker_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN ITEM'S OWN DETAIL NAMES THE TASKS ITS HISTORY POINTS AT.
//
// `TestAReparentRecordsBothParentsByID` holds the company-wide log, which
// resolved a re-parent to "Parent: — → ENG-1". This is the screen a reader
// actually opens to ask what happened to THIS task, and for the same commit it
// rendered "Parent: — → 1d573f85-…": the answer carried no map, so the
// dashboard had nothing to resolve the id with.
//
// The map is NOT a second implementation — the detail read collects the sides
// of its own history rows and hands them to the same walk the feed uses — so
// the one thing this asserts that the case above cannot is that the detail
// read ASKS, and that the raw snapshot shape its history keeps is collected
// from at all.
//
// IT DEPENDS ON `History`, because the map labels those rows: a caller that
// did not ask for them gets no map, and the field is omitted rather than
// carrying a resolution of nothing.
func TestATaskDetailResolvesTheParentItsHistoryNames(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "t-parent")
	filedTask(t, r, "t-child")

	parent := "t-parent"
	if _, err := r.writer.UpdateTask(t.Context(), "op-adopt", "t-child", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Parent: &parent},
		tracker.ChangeReparented, nil); err != nil {
		t.Fatalf("re-parent: %v", err)
	}
	r.drain()

	// READ OFF THE ROW, because the key is minted by the create from the
	// project's own counter — what the fixture calls the task is not what
	// it is called.
	want := r.task(t, "t-parent").Task.Key
	if want == "" {
		t.Fatal("the parent has no key, so this case asserts nothing")
	}

	detail, err := r.reader.Task(t.Context(), "t-child",
		tracker.DetailWants{History: true},
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
	bare, err := r.reader.Task(t.Context(), "t-child", tracker.DetailWants{},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the child without its history: %v", err)
	}
	if bare.Keys != nil {
		t.Errorf("a detail read with no history carries %d keys", len(bare.Keys))
	}
}
