package worklet_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// fakeRanker is an index that ranks the keys it was given, best first, with
// the item each names.
type fakeRanker struct {
	lexical  []search.Scored
	docs     map[string]worklet.RankedDoc
	building bool
}

// Candidates answers the keys it was given as a keyword ranking: the words
// were ranked, which is what makes an empty answer ask the building gate.
func (f fakeRanker) Candidates(context.Context, worklet.SearchQuery) (worklet.RankedCandidates, error) {
	return worklet.RankedCandidates{
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
		docs: map[string]worklet.RankedDoc{
			"task:gone": {ID: gone.ID}, "task:a": {ID: keep1.ID, Snippet: "backoff"},
			"task:b": {ID: keep2.ID},
		},
	}
	answer, err := worklet.NewSearcher(r.db.Reader(), rank).Search(t.Context(),
		worklet.SearchQuery{Text: "retry", Limit: 2})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	got := answer.Hits
	if len(got) != 2 || got[0].ID != keep1.ID || got[1].ID != keep2.ID {
		t.Fatalf("ranked %+v, want the two surviving items, the removed leader walked past", got)
	}
	if got[0].Rank != 1 || got[1].Rank != 2 || got[0].Snippet != "backoff" {
		t.Errorf("ranked %+v, want places 1 and 2 and the index's excerpt", got)
	}
}

// AN EMPTY RANKING FROM AN INDEX STILL BUILDING IS NOT "NOTHING MATCHED": it is
// [worklet.ErrIndexBuilding], which a seat is told as "ask again" rather than
// acting on by filing a duplicate — and a ranking that found something has
// found it, building or not.
func TestAnEmptyRankingFromABuildingIndexSaysSo(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("Retry backoff")
	_, err := worklet.NewSearcher(r.db.Reader(), fakeRanker{building: true}).Search(t.Context(),
		worklet.SearchQuery{Text: "retry"})
	if !errors.Is(err, worklet.ErrIndexBuilding) {
		t.Fatalf("an empty ranking from a building index = %v, want ErrIndexBuilding", err)
	}
	found := fakeRanker{building: true,
		lexical: []search.Scored{{Key: "task:a", Score: 1}},
		docs:    map[string]worklet.RankedDoc{"task:a": {ID: task.ID}}}
	answer, err := worklet.NewSearcher(r.db.Reader(), found).Search(t.Context(),
		worklet.SearchQuery{Text: "retry"})
	if err != nil || !slices.ContainsFunc(answer.Hits,
		func(row worklet.Ranked) bool { return row.ID == task.ID }) {
		t.Fatalf("a ranking that found an item while building = (%+v, %v), want the item", answer, err)
	}
}

// AN EXACT KEY IS ITS TASK, AND IT LEADS: the index ranks words, and a key
// split into "eng" and "2" ranks every body that mentions it above the task
// itself. Typed in any case, the key's task comes first and is not listed
// again where the ranking also found it; text that is more than one key is
// ranked as words.
func TestAnExactKeyLeadsTheRankingAndIsNotListedTwice(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	target, other := r.createTask("Retry backoff"), r.createTask("Subtask of the retry work")
	rank := fakeRanker{
		lexical: []search.Scored{{Key: "task:other", Score: 9}, {Key: "task:target", Score: 1}},
		docs: map[string]worklet.RankedDoc{
			"task:other": {ID: other.ID}, "task:target": {ID: target.ID},
		},
	}
	searcher := worklet.NewSearcher(r.db.Reader(), rank)
	answer, err := searcher.Search(t.Context(),
		worklet.SearchQuery{Text: " " + strings.ToLower(target.Key) + " ", Limit: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	got := answer.Hits
	if len(got) != 2 || got[0].ID != target.ID || got[1].ID != other.ID {
		t.Fatalf("searching %q answered %+v, want %s first and %s once after it",
			target.Key, got, target.Key, other.Key)
	}
	if got[0].Rank != 1 || got[1].Rank != 2 || got[0].Key != target.Key {
		t.Errorf("answered %+v, want places 1 and 2 under the task's own key", got)
	}

	words, err := searcher.Search(t.Context(),
		worklet.SearchQuery{Text: target.Key + " follow-up", Limit: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(words.Hits) != 2 || words.Hits[0].ID != other.ID {
		t.Errorf("words that are not one key answered %+v, want the ranking's own order", words.Hits)
	}
}

// A KEY A MOVE LEFT BEHIND STILL FINDS ITS TASK, as it still opens it: the
// directory holds every key a task answers to, and an empty ranking does not
// hide the task it names.
func TestAFormerKeyLeadsASearchUnderTheTasksCurrentKey(t *testing.T) {
	t.Parallel()
	r := moveFixture(t)
	before, err := r.reader.Task(t.Context(), "m-root", worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read the root: %v", err)
	}
	if _, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil); err != nil {
		t.Fatalf("move: %v", err)
	}
	r.drain()
	answer, err := worklet.NewSearcher(r.db.Reader(), fakeRanker{}).Search(t.Context(),
		worklet.SearchQuery{Text: before.Task.Key})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(answer.Hits) != 1 || answer.Hits[0].ID != "m-root" ||
		!strings.HasPrefix(answer.Hits[0].Key, "OPS-") {
		t.Fatalf("searching the former key %s answered %+v, want the task under its OPS key",
			before.Task.Key, answer.Hits)
	}
}
