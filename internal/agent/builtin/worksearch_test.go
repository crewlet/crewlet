package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A SEAT CAN FIND AN ITEM BY WHAT ITS DESCRIPTION SAYS, which the board's
// `text` (a substring of key or title) cannot reach.
func TestASeatCanSearchWorkItemsByWhatTheySay(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.ranked = []worklet.Ranked{
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

// A BUILDING INDEX IS NOT AN EMPTY ANSWER: a model acts on "there is nothing"
// by filing a duplicate, so a node that has not caught up says so.
func TestASearchOnABuildingIndexSaysSoRatherThanAnsweringEmpty(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.searchErr = worklet.ErrIndexBuilding
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Search: trk})

	got := callWork(t, reg, builtin.SearchWorkItemsTool, map[string]any{"text": "anything"})
	if !got.Failed {
		t.Fatalf("a building index answered %q as though it were a result",
			got.Output)
	}
	// The tool's own sentence, saying the answer says NOTHING about whether
	// the work exists, not the sentinel wrapped in a generic read failure.
	if !strings.Contains(got.Output, "says nothing about whether the work exists") {
		t.Errorf("the refusal is %q — a model reading a generic read failure "+
			"here retries the call, where what it needs to know is that the "+
			"silence is about the index rather than about the company",
			got.Output)
	}
}

// A BUILD WITH NO INDEX DOES NOT ADVERTISE THE VERB, on [Register]'s rule: a
// tool that always fails teaches a model to distrust the catalogue.
func TestNoIndexMeansNoSearchTool(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	if _, held := reg.Lookup(builtin.SearchWorkItemsTool); held {
		t.Error("search_work_items was registered with no index behind it")
	}
}

// A RANKED ANSWER SAYS WHAT THE RANKING IS: a place, since the fan-out answers
// fused keys best first and no score survives to the coordinator.
func TestARankedAnswerNumbersItsPlaces(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.ranked = []worklet.Ranked{
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

// A SEAT'S SEARCH ASKS FOR HYBRID and says when it covered part of the corpus,
// or a model concludes the items it was not shown do not exist.
func TestAPartialWorkSearchSaysSoToTheSeat(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.ranked = []worklet.Ranked{{ID: "i1", Key: "ENG-7", Title: "Flaky checkout"}}
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
// The mode is passed through, not fixed. An unknown mode is refused by name,
// since the default answers another question, and a ranking the company cannot
// serve is SAID: a semantic search with no embeddings ran nothing, and an
// unexplained empty list reads as "no such work".
func TestAWorkSearchTakesItsModeAndSaysWhenItWasNotServed(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.ranked = []worklet.Ranked{{ID: "i1", Key: "ENG-7", Title: "Flaky checkout"}}
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

// A TEXT PAST THE SEARCH'S BOUND IS REFUSED NAMING IT, and never reaches the
// search, whose error reads as the tracker failing. At the bound it is a search.
func TestALongWorkSearchIsRefusedNotCut(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Search: trk})

	got := callWork(t, reg, builtin.SearchWorkItemsTool, map[string]any{
		"text": strings.Repeat("w", knowledge.MaxQueryBytes+1),
	})
	if !got.Failed || !strings.Contains(got.Output, "takes at most 400") {
		t.Fatalf("a long text was not refused naming the limit: %q", got.Output)
	}
	if len(trk.searched) != 0 {
		t.Fatal("a refused text still reached the search")
	}
	if got := callWork(t, reg, builtin.SearchWorkItemsTool, map[string]any{
		"text": strings.Repeat("w", knowledge.MaxQueryBytes),
	}); got.Failed {
		t.Fatalf("a text at the bound was refused: %q", got.Output)
	}
}
