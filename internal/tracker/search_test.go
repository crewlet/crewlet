package tracker_test

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// rankerStub is a scripted index: what it ranks, what that ranking is
// missing, whether it is still on its first build, and how many rankings it
// was asked for.
type rankerStub struct {
	docs     []tracker.RankedDoc
	partial  *tracker.SearchPartial
	building bool
	asked    []int
}

func (r *rankerStub) RankItems(_ context.Context, _ string, limit int) (
	[]tracker.RankedDoc, *tracker.SearchPartial, error) {

	r.asked = append(r.asked, limit)
	return r.docs, r.partial, nil
}

func (r *rankerStub) Building(context.Context) bool { return r.building }

// searchStore is a node's store holding one work item, ENG-1.
func searchStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Replicated().SQL().ExecContext(t.Context(), `
		INSERT INTO tracker_tasks (id, key, project_key, root_id, type, title,
		                           status, status_group, rank, document,
		                           version, created_at, updated_at)
		VALUES ('t.1', 'ENG-1', 'ENG', 't.1', 'task', 'Rotate the signing key',
		        'todo', 'not_started', 'a0', json_object('body', ''), 1, 0, 0)`); err != nil {
		t.Fatalf("insert a work item: %v", err)
	}
	return db
}

// A BUILDING INDEX IS REFUSED, WHATEVER IT FOUND.
//
// While this node's index is on its first build, a ranking it returns is one
// it cannot vouch for: where the fleet divides a search it counts its own
// buckets missing and drops what a peer ranked that it has not indexed yet.
// The answer is rows with nothing beside them, so a partial one reads as the
// whole, and a seat that takes a short list for every match files the
// duplicate. The control is the same ranking once the index is built.
func TestABuildingIndexIsRefusedWhateverItFound(t *testing.T) {
	t.Parallel()
	db := searchStore(t)
	ranker := &rankerStub{building: true,
		docs: []tracker.RankedDoc{{ID: "t.1", Snippet: "the signing key"}}}
	items := tracker.NewSearcher(db, ranker)

	got, err := items.Search(t.Context(), "signing key", 0)
	if !errors.Is(err, tracker.ErrIndexBuilding) {
		t.Fatalf("a search on a building index answered %+v, %v; want %v",
			got, err, tracker.ErrIndexBuilding)
	}
	if len(ranker.asked) != 0 {
		t.Errorf("a building index was ranked anyway, %d time(s)", len(ranker.asked))
	}

	ranker.building = false
	got, err = items.Search(t.Context(), "signing key", 0)
	if err != nil {
		t.Fatalf("a search on a built index: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Key != "ENG-1" || got.Items[0].Rank != 1 {
		t.Errorf("a built index answered %+v, want ENG-1 in first place", got.Items)
	}
}

// A PARTIAL RANKING COMES BACK WITH ITS ITEMS AND WHAT IT IS MISSING.
//
// The items are real — a peer that did not answer costs its share of the
// corpus, not the answer — and a list of them with nothing beside it reads as
// every match there is, which a seat acts on by filing the duplicate. So the
// ranking's own count travels with the rows, whether or not any of them
// survived the read.
func TestAPartialRankingIsCarriedWithItsItems(t *testing.T) {
	t.Parallel()
	db := searchStore(t)
	partial := &tracker.SearchPartial{
		BucketsAnswered: 42, BucketsMissing: 22, AbsentNodes: []string{"node-b"},
	}
	ranker := &rankerStub{partial: partial,
		docs: []tracker.RankedDoc{{ID: "t.1", Snippet: "the signing key"}}}
	items := tracker.NewSearcher(db, ranker)

	got, err := items.Search(t.Context(), "signing key", 0)
	if err != nil {
		t.Fatalf("a partial ranking was refused: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Key != "ENG-1" {
		t.Errorf("the partial ranking's items are %+v, want ENG-1", got.Items)
	}
	if got.Partial != partial {
		t.Errorf("the answer carries partial %+v, want the ranking's own %+v",
			got.Partial, partial)
	}

	// AND WHEN NOTHING SURVIVED, the count still travels: an empty list
	// over part of the corpus is not "no such work".
	ranker.docs = []tracker.RankedDoc{{ID: "t.gone"}}
	got, err = items.Search(t.Context(), "signing key", 0)
	if err != nil {
		t.Fatalf("a partial ranking of a removed item was refused: %v", err)
	}
	if got.Items == nil || len(got.Items) != 0 || got.Partial != partial {
		t.Errorf("an empty partial answer is %+v — want an empty list carrying "+
			"the ranking's count", got)
	}
	ranker.docs = nil
	if got, _ = items.Search(t.Context(), "signing key", 0); got.Items == nil ||
		got.Partial != partial {
		t.Errorf("a ranking with no items is %+v — want an empty list carrying "+
			"the ranking's count", got)
	}
}

// AN ASK PAST THE CEILING IS REFUSED NAMING IT, never answered with fewer.
//
// A list of fifty handed to a caller that asked for a hundred reads as all the
// hundred it asked for there were. At the ceiling the ask is served, and no
// limit at all takes the default.
func TestAnAskPastTheCeilingIsRefusedNamingIt(t *testing.T) {
	t.Parallel()
	db := searchStore(t)
	ranker := &rankerStub{docs: []tracker.RankedDoc{{ID: "t.1"}}}
	items := tracker.NewSearcher(db, ranker)

	over := tracker.MaxSearchLimit + 1
	_, err := items.Search(t.Context(), "signing key", over)
	if !errors.Is(err, tracker.ErrSearchLimit) {
		t.Fatalf("an ask for %d answered %v, want %v", over, err, tracker.ErrSearchLimit)
	}
	for _, want := range []string{"`limit`", strconv.Itoa(over),
		strconv.Itoa(tracker.MaxSearchLimit)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %s", err, want)
		}
	}
	if len(ranker.asked) != 0 {
		t.Errorf("a refused ask was ranked anyway: %v", ranker.asked)
	}

	for _, tc := range []struct{ ask, want int }{
		{tracker.MaxSearchLimit, tracker.MaxSearchLimit},
		{0, tracker.SearchLimit},
	} {
		ranker.asked = nil
		if _, err := items.Search(t.Context(), "signing key", tc.ask); err != nil {
			t.Fatalf("an ask for %d was refused: %v", tc.ask, err)
		}
		if len(ranker.asked) != 1 || ranker.asked[0] != tc.want {
			t.Errorf("an ask for %d ranked %v, want %d", tc.ask, ranker.asked, tc.want)
		}
	}
}
