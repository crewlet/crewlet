package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/knowledge"
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

// A RANKED ANSWER SAYS WHAT THE RANKING IS.
//
// It carried a `score` that could only ever be zero: the fan-out's answer is
// fused KEYS, best first — the arithmetic that ordered them is finished before
// a coordinator sees them — so nothing downstream had a score to set. A place
// is what the answer actually has, and it is what a caller can act on.
func TestARankedAnswerNumbersItsPlaces(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.ranked = []tracker.Ranked{
		{ID: "i1", Key: "ENG-7", Title: "first", Rank: 1},
		{ID: "i2", Key: "ENG-9", Title: "second", Rank: 2},
	}
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Search: trk})

	got := callWork(t, reg, builtin.SearchWorkItemsTool, map[string]any{"text": "anything"})
	if got.Failed {
		t.Fatalf("the search failed: %s", got.Output)
	}
	if strings.Contains(got.Output, `"score"`) {
		t.Errorf("the answer still carries a score nothing can set: %s", got.Output)
	}
	if !strings.Contains(got.Output, `"rank": 1`) || !strings.Contains(got.Output, `"rank": 2`) {
		t.Errorf("the answer is %q and does not number its places", got.Output)
	}
}

// A SEAT'S SEARCH ASKS FOR HYBRID and says when it covered part of the corpus.
//
// The partial answer used to be a log line on the node that coordinated it, so
// a model reading five items out of what should have been eight concluded the
// other three did not exist. The coverage now rides in the answer, and the
// tool is the one place a model can be told.
func TestAPartialWorkSearchSaysSoToTheSeat(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.ranked = []tracker.Ranked{{ID: "i1", Key: "ENG-7", Title: "Flaky checkout"}}
	trk.partialSearch = true
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Search: trk})

	got := callWork(t, reg, builtin.SearchWorkItemsTool, map[string]any{"text": "checkout"})
	if got.Failed {
		t.Fatalf("the search failed: %s", got.Output)
	}
	if !strings.Contains(got.Output, "only part of the company's work") {
		t.Errorf("a partial answer reached the seat as %q, which reads exactly "+
			"like a complete one", got.Output)
	}
	if len(trk.searchModes) != 1 || trk.searchModes[0].Resolved() != knowledge.ModeHybrid {
		t.Errorf("the tool searched in %v, want the hybrid default", trk.searchModes)
	}

	complete := newFakeTracker()
	complete.ranked = trk.ranked
	reg = workRegistry(t, builtin.WorkDeps{Reader: complete, Writer: complete.as, Search: complete})
	if got := callWork(t, reg, builtin.SearchWorkItemsTool,
		map[string]any{"text": "checkout"}); strings.Contains(got.Output, "partial") {
		t.Errorf("a complete answer carries a partial note: %q", got.Output)
	}
}

// A SEARCH RANKS THE WAY IT WAS ASKED TO, and says when it could not.
//
// The mode is the same three values `search_knowledge` and the dashboard take,
// passed through rather than fixed: a seat looking for "the item that says
// ERR_TIMEOUT_42" wants the words alone, and one looking for work described in
// other words wants meaning alone. A mode the build does not know is refused by
// name, because answering it as the default answers another question. And a
// ranking the company cannot serve is SAID — a semantic search on a company
// with no embeddings ran nothing, and an empty list beside no word about it
// reads as "no such work".
func TestAWorkSearchTakesItsModeAndSaysWhenItWasNotServed(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.ranked = []tracker.Ranked{{ID: "i1", Key: "ENG-7", Title: "Flaky checkout"}}
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Search: trk})

	if got := callWork(t, reg, builtin.SearchWorkItemsTool, map[string]any{
		"text": "ERR_TIMEOUT_42", "mode": "keyword",
	}); got.Failed || !strings.Contains(got.Output, `"mode": "keyword"`) {
		t.Fatalf("a keyword search answered %q", got.Output)
	}
	if len(trk.searchModes) != 1 || trk.searchModes[0] != knowledge.ModeKeyword {
		t.Fatalf("the tool searched in %v, want keyword as asked", trk.searchModes)
	}

	bad := callWork(t, reg, builtin.SearchWorkItemsTool, map[string]any{
		"text": "checkout", "mode": "fuzzy",
	})
	if !bad.Failed || !strings.Contains(bad.Output, "fuzzy") {
		t.Errorf("an unknown mode was not refused by name: %s", bad.Output)
	}
	if len(trk.searchModes) != 1 {
		t.Errorf("the refused mode still searched: %v", trk.searchModes)
	}

	unserved := newFakeTracker()
	unserved.ranked = trk.ranked
	unserved.searchDegraded = knowledge.DegradedNoEmbeddings
	reg = workRegistry(t, builtin.WorkDeps{Reader: unserved, Writer: unserved.as,
		Search: unserved})
	semantic := callWork(t, reg, builtin.SearchWorkItemsTool, map[string]any{
		"text": "checkout", "mode": "semantic",
	})
	if semantic.Failed || !strings.Contains(semantic.Output, "no search by meaning ran") {
		t.Errorf("a semantic search that ran nothing answered %q, which reads "+
			"as no such work", semantic.Output)
	}
	hybrid := callWork(t, reg, builtin.SearchWorkItemsTool, map[string]any{
		"text": "checkout",
	})
	if hybrid.Failed || !strings.Contains(hybrid.Output, "ranked by the words alone") {
		t.Errorf("a hybrid search served by the words alone answered %q without "+
			"saying so", hybrid.Output)
	}
}
