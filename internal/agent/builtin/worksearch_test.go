package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A SEAT CAN FIND AN ITEM BY WHAT IT SAYS, and until this it could not.
//
// The board's `text` is a substring of the key or the title, so every word
// somebody wrote in a DESCRIPTION was unreachable — while the engine embedded
// every one of those descriptions on a fleet-singleton duty and stored a
// replicated vector for each, which nothing ever read.
func TestASeatCanSearchWorkItemsByWhatTheySay(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.ranked = []tracker.Ranked{
		{ID: "i1", Key: "ENG-7", Title: "Flaky checkout", Snippet: "…no backoff…"},
	}
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Search: trk})

	got := callWork(t, reg, builtin.SearchWorkItemsTool, map[string]any{
		"text": "retry backoff on the payment client",
	})
	if got.Failed {
		t.Fatalf("the search failed: %s", got.Output)
	}
	if len(trk.searched) != 1 || trk.searched[0] != "retry backoff on the payment client" {
		t.Fatalf("the tool searched for %v", trk.searched)
	}
	if !strings.Contains(got.Output, "ENG-7") {
		t.Errorf("the answer is %q and does not name the item's KEY — which "+
			"is what a caller pastes into every other verb here", got.Output)
	}
}

// A BUILDING INDEX IS NOT AN EMPTY ANSWER.
//
// "There is nothing" is what a model acts on by filing a duplicate, which is
// the one outcome the whole tracker exists to prevent. A node that has not
// caught up has to say so.
func TestASearchOnABuildingIndexSaysSoRatherThanAnsweringEmpty(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.searchErr = tracker.ErrIndexBuilding
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Search: trk})

	got := callWork(t, reg, builtin.SearchWorkItemsTool, map[string]any{"text": "anything"})
	if !got.Failed {
		t.Fatalf("a building index answered %q as though it were a result",
			got.Output)
	}
	// THE TOOL'S OWN SENTENCE, not the sentinel's text wrapped in a generic
	// read failure: what a model has to be told is that the answer says
	// NOTHING about whether the work exists, and a failure that merely
	// quotes "the index is still building" reads as this call being broken.
	if !strings.Contains(got.Output, "says nothing about whether the work exists") {
		t.Errorf("the refusal is %q — a model reading a generic read failure "+
			"here retries the call, where what it needs to know is that the "+
			"silence is about the index rather than about the company",
			got.Output)
	}
}

// A BUILD WITH NO INDEX DOES NOT ADVERTISE THE VERB, on [Register]'s own rule:
// a company on another tracker has no corpus here, and a catalogue offering a
// tool that always fails is how a model learns to distrust all of them.
func TestNoIndexMeansNoSearchTool(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	if _, held := reg.Lookup(builtin.SearchWorkItemsTool); held {
		t.Error("search_work_items was registered with no index behind it")
	}
}
