package tracker_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/tracker"
)

// fakeRanker is an index that ranks the keys it was given, best first, with
// the item each names.
type fakeRanker struct {
	lexical  []search.Scored
	docs     map[string]tracker.RankedDoc
	building bool
}

// Candidates answers the keys it was given as a keyword ranking: the words
// were ranked, which is what makes an empty answer ask the building gate.
func (f fakeRanker) Candidates(context.Context, tracker.SearchQuery) (tracker.RankedCandidates, error) {
	return tracker.RankedCandidates{
		Candidates: search.Candidates{Lexical: f.lexical}, Docs: f.docs,
		Outcome: knowledge.Outcome{ServedMode: knowledge.ModeKeyword},
	}, nil
}

func (f fakeRanker) Building(context.Context) bool { return f.building }

// A RANKED SEARCH WALKS PAST AN ITEM REMOVED SINCE IT WAS INDEXED, and the
// places it gives are counted over what survives — so a removed leader costs
// the answer that item and never a place, and the numbering has no gap a
// reader would take for a result that went missing.
func TestARankedSearchWalksPastARemovedItemWithoutAGap(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	keep1, gone, keep2 := r.createTask("Retry backoff"), r.createTask("Retry jitter"),
		r.createTask("Retry budget")
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", gone.ID, "ENG", false, nil); err != nil {
		t.Fatalf("remove: %v", err)
	}
	r.drain()
	rank := fakeRanker{
		lexical: []search.Scored{{Key: "task:gone", Score: 9}, {Key: "task:a", Score: 5},
			{Key: "task:b", Score: 1}},
		docs: map[string]tracker.RankedDoc{
			"task:gone": {ID: gone.ID}, "task:a": {ID: keep1.ID, Snippet: "backoff"},
			"task:b": {ID: keep2.ID},
		},
	}
	slice, err := tracker.NewSearcher(r.db.Reader(), rank).Slice(t.Context(), tracker.SearchQuery{Text: "retry"})
	if err != nil {
		t.Fatalf("slice: %v", err)
	}
	got := tracker.MergeSearch([]tracker.SearchSlice{slice}, tracker.SearchQuery{Limit: 2}).Hits
	if len(got) != 2 || got[0].ID != keep1.ID || got[1].ID != keep2.ID {
		t.Fatalf("ranked %+v, want the two surviving items, the removed leader walked past", got)
	}
	if got[0].Rank != 1 || got[1].Rank != 2 || got[0].Snippet != "backoff" {
		t.Errorf("ranked %+v, want places 1 and 2 and the index's excerpt", got)
	}
}

// AN EMPTY RANKING FROM AN INDEX STILL BUILDING IS NOT "NOTHING MATCHED": it is
// [tracker.ErrIndexBuilding], which a seat is told as "ask again" rather than
// acting on by filing a duplicate — and a ranking that found something has
// found it, building or not.
func TestAnEmptyRankingFromABuildingIndexSaysSo(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("Retry backoff")
	_, err := tracker.NewSearcher(r.db.Reader(), fakeRanker{building: true}).Slice(t.Context(), tracker.SearchQuery{Text: "retry"})
	if !errors.Is(err, tracker.ErrIndexBuilding) {
		t.Fatalf("an empty ranking from a building index = %v, want ErrIndexBuilding", err)
	}
	found := fakeRanker{building: true,
		lexical: []search.Scored{{Key: "task:a", Score: 1}},
		docs:    map[string]tracker.RankedDoc{"task:a": {ID: task.ID}}}
	slice, err := tracker.NewSearcher(r.db.Reader(), found).Slice(t.Context(), tracker.SearchQuery{Text: "retry"})
	if err != nil || !slices.ContainsFunc(tracker.MergeSearch([]tracker.SearchSlice{slice}, tracker.SearchQuery{}).Hits,
		func(row tracker.Ranked) bool { return row.ID == task.ID }) {
		t.Fatalf("a ranking that found an item while building = (%+v, %v), want the item", slice, err)
	}
}
