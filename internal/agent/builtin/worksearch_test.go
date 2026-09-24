package builtin_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A SEAT CAN FIND AN ITEM BY WHAT IT SAYS.
//
// The board's `text` is a substring of the key or the title, so every word
// somebody wrote in a DESCRIPTION is reachable only by the ranked search.
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

// AN ASK PAST THE CEILING IS REFUSED AS WHAT IT IS: the argument's fault, named.
//
// The tracker refuses a `limit` above [tracker.MaxSearchLimit] naming the
// field and the most it takes. Reported as a read failure, that is "could not
// read the tracker right now … try again" — which sends a model round the same
// call — so the tool hands the tracker's own sentence back instead. Asked
// through the real searcher, whose refusal comes before it reads anything.
func TestAnAskPastTheSearchCeilingIsRefusedNamingIt(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	// AN UNOPENED STORE HANDLE, because the limit is refused before the
	// store is read — and a ranking reached here fails the case loudly.
	items := tracker.NewSearcher(&store.DB{}, rankerThatMustNotRun{t: t})
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Search: items})

	over := tracker.MaxSearchLimit + 1
	got := callWork(t, reg, builtin.SearchWorkItemsTool, map[string]any{
		"text": "anything", "limit": float64(over),
	})
	if !got.Failed {
		t.Fatalf("an ask for %d was answered: %s", over, got.Output)
	}
	for _, want := range []string{"`limit`", strconv.Itoa(over),
		strconv.Itoa(tracker.MaxSearchLimit)} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the refusal %q does not name %s", got.Output, want)
		}
	}
	if strings.Contains(got.Output, "try again") || strings.Contains(got.Output, "Try again") {
		t.Errorf("the refusal tells the model to retry the same call: %q", got.Output)
	}
}

// rankerThatMustNotRun is an index a case expects never to be asked.
type rankerThatMustNotRun struct{ t *testing.T }

func (r rankerThatMustNotRun) RankItems(context.Context, string, int) (
	[]tracker.RankedDoc, *tracker.SearchPartial, error) {

	r.t.Error("the index was asked to rank a search the tracker should have refused")
	return nil, nil, nil
}

func (rankerThatMustNotRun) Building(context.Context) bool { return false }

// A PARTIAL RANKING SAYS WHAT IT IS MISSING, beside what it found.
//
// A peer that did not answer in time costs its share of the corpus, and a
// query the meaning half could not run is ranked on its words alone. The items
// are real either way, and a list of them with nothing beside it reads as
// every match there is — which a model acts on by filing the duplicate. A
// whole ranking carries no such field, because a field that is always there is
// one nobody reads.
func TestAPartialRankingSaysWhatItIsMissing(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.ranked = []tracker.Ranked{{ID: "i1", Key: "ENG-7", Title: "first", Rank: 1}}
	trk.partial = &tracker.SearchPartial{BucketsAnswered: 42, BucketsMissing: 22,
		AbsentNodes: []string{"node-b"}}
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Search: trk})

	got := callWork(t, reg, builtin.SearchWorkItemsTool, map[string]any{"text": "anything"})
	if got.Failed {
		t.Fatalf("a partial ranking failed the call: %s", got.Output)
	}
	for _, want := range []string{`"partial"`, `"buckets_missing": 22`,
		`"absent_nodes"`, "node-b", "ENG-7"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the answer does not carry %s: %s", want, got.Output)
		}
	}

	trk.partial = nil
	whole := callWork(t, reg, builtin.SearchWorkItemsTool, map[string]any{"text": "anything"})
	if strings.Contains(whole.Output, `"partial"`) {
		t.Errorf("a whole ranking carries a partial field: %s", whole.Output)
	}
}

// A RANKED ANSWER SAYS WHAT THE RANKING IS: a place, never a score.
//
// The fan-out's answer is fused KEYS, best first — the arithmetic that ordered
// them is finished before a coordinator sees them — so nothing downstream has
// a score to set. A place is what the answer actually has, and it is what a
// caller can act on.
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
