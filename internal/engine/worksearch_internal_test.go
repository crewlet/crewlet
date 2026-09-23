package engine

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A WORK SEARCH WAITS FOR THE WORK ITEMS' FIRST BUILD AND FOR NOTHING ELSE.
//
// The index covers the pages as well as the work items, and each corpus
// finishes its first lap on its own. The tracker's searcher asks the ranker
// whether it is building whenever an answer comes back empty, and a ranker
// that asked about the whole index would answer every empty work search
// "the index is still building" for as long as the PAGES were on their first
// lap — which a seat reads as "ask again" about a corpus that was ready. The
// second source here stands for the page corpus, and never finishes a lap.
func TestAWorkSearchWaitsForTheItemsFirstBuildAlone(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), t.TempDir()+"/index.db", store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Replicated().SQL().ExecContext(t.Context(), `
		INSERT INTO tracker_tasks (id, key, project_key, root_id, type, title,
		                           status, status_group, rank, document,
		                           version, created_at, updated_at)
		VALUES ('t.1', 'ENG-1', 'ENG', 't.1', 'task', 'Rotate the signing key',
		        'todo', 'not_started', 'a0', json_object('body', 'before it expires'),
		        1, 0, 0)`); err != nil {
		t.Fatalf("insert a work item: %v", err)
	}
	index := search.NewIndexerOver(db,
		[]search.LexicalSource{search.TaskSource{}, unfinishedCorpus{}})
	items := tracker.NewSearcher(db, itemRanker{index: index, fan: &search.FanOut{
		Self: "self", Local: search.NodeScanner{Index: index},
	}})

	// THE CONTROL: before any lap the gate is closed, so what opens it
	// below is the work items' lap rather than a gate that never closes.
	if _, err := items.Search(t.Context(), "nothing matches this", 0); !errors.Is(err, tracker.ErrIndexBuilding) {
		t.Fatalf("a search over an index that has built nothing answered %v, want %v",
			err, tracker.ErrIndexBuilding)
	}

	// The work items come first, so their lap finishes before the sweep
	// reaches the corpus that never does — which then fails every sweep.
	for sweeps := 0; !index.ReadyFor(itemCorpus); sweeps++ {
		if sweeps == 100 {
			t.Fatal("the work items never finished their first lap")
		}
		if _, err := index.Sweep(t.Context()); err != nil && !errors.Is(err, errUnfinishedCorpus) {
			t.Fatalf("index the work items: %v", err)
		}
	}
	if index.ReadyFor() {
		t.Fatal("the unfinished corpus reports its first build done, so this " +
			"case cannot tell one corpus's gate from the whole index's")
	}

	if _, err := items.Search(t.Context(), "nothing matches this", 0); err != nil {
		t.Errorf("an empty work search on built items answered %v, want an empty "+
			"answer — the gate is waiting on a corpus the search does not read", err)
	}
	got, err := items.Search(t.Context(), "signing key", 0)
	if err != nil {
		t.Fatalf("search the work items: %v", err)
	}
	if len(got) != 1 || got[0].Key != "ENG-1" {
		t.Errorf("the search returned %+v, want ENG-1", got)
	}
}

// errUnfinishedCorpus is what [unfinishedCorpus] answers every scan with.
var errUnfinishedCorpus = errors.New("this corpus has not finished its first lap")

// unfinishedCorpus is a corpus whose first lap never finishes: every scan of it
// fails, so the index never marks it built.
type unfinishedCorpus struct{}

func (unfinishedCorpus) Source() string { return string(search.SourcePage) }

func (unfinishedCorpus) Versions(context.Context, *sql.Tx, string, int) ([]search.DocVersion, error) {
	return nil, errUnfinishedCorpus
}

func (unfinishedCorpus) Fetch(context.Context, *sql.Tx, []string) ([]search.Doc, error) {
	return nil, errUnfinishedCorpus
}

func (unfinishedCorpus) Live(context.Context, *sql.Tx, []string) (map[string]bool, error) {
	return nil, errUnfinishedCorpus
}

func (unfinishedCorpus) Count(context.Context, *sql.Tx) (int, error) {
	return 0, errUnfinishedCorpus
}

// THE INDEX FOLLOWS BOTH BACKENDS, not the wiki's alone.
//
// The lexical index covers pages AND work items. Built only where
// `knowledge.backend: native`, a company running the engine's own tracker with
// Confluence for its knowledge would index none of its own work items and
// serve no ranked item search.
func TestTheIndexFollowsEitherNativeBackend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		tracker, wiki bool
		want          []string
	}{
		{"both", true, true, []string{"task", "page"}},
		{"tracker only, Confluence knowledge", true, false, []string{"task"}},
		{"wiki only, Jira tracker", false, true, []string{"page"}},
		{"neither", false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := indexedCorpora(tc.tracker, tc.wiki)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("a node with tracker=%v wiki=%v indexes %v, want %v",
					tc.tracker, tc.wiki, got, tc.want)
			}
		})
	}
}

// AND THE SOURCES THE INDEXER IS ACTUALLY BUILT OVER ARE THOSE ONES.
//
// Without this, [indexedCorpora] would be a list nothing reads — a test that
// asserts a function against itself while the startup goes on doing whatever
// it did before, which is the exact shape of defect this whole file is about.
func TestTheIndexerIsBuiltOverTheCorporaTheBackendsName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ tracker, wiki bool }{
		{true, true}, {true, false}, {false, true}, {false, false},
	} {
		want := indexedCorpora(tc.tracker, tc.wiki)
		var got []string
		for _, source := range lexicalSources(tc.tracker, tc.wiki) {
			got = append(got, source.Source())
		}
		if !slices.Equal(got, want) {
			t.Errorf("tracker=%v wiki=%v builds the index over %v and names %v",
				tc.tracker, tc.wiki, got, want)
		}
	}
}
