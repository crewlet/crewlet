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
// whether it is building on every search, and a ranker that asked about the
// whole index would refuse every work search "the index is still building"
// for as long as the PAGES were on their first lap — which a seat reads as
// "ask again" about a corpus that was ready. The second source here stands
// for the page corpus, and never finishes a lap.
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
	// below is the work items' lap rather than a gate that was open all
	// along.
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
	if len(got.Items) != 1 || got.Items[0].Key != "ENG-1" {
		t.Errorf("the search returned %+v, want ENG-1", got.Items)
	}
}

// A WORK SEARCH A PEER DID NOT COVER SAYS SO, through the tracker's own seam.
//
// A peer that replied its own index is still building covered none of its
// bucket range, and the fan-out counts that range missing. The items this node
// ranked from its own range are real, so they come back — with the ranking's
// count beside them, naming the peer, because a short list with nothing beside
// it reads as every match there is and a seat acts on that by filing the
// duplicate. The control is the same search with the peer answering.
func TestAWorkSearchAPeerDidNotCoverSaysSo(t *testing.T) {
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
	index := search.NewIndexerOver(db, []search.LexicalSource{search.TaskSource{}})
	for sweeps := 0; !index.ReadyFor(itemCorpus); sweeps++ {
		if sweeps == 100 {
			t.Fatal("the work items never finished their first lap")
		}
		if _, err := index.Sweep(t.Context()); err != nil {
			t.Fatalf("index the work items: %v", err)
		}
	}
	ask := func(peer search.Slice) tracker.Ranking {
		t.Helper()
		items := tracker.NewSearcher(db, itemRanker{index: index, fan: &search.FanOut{
			Self: "node-a", Local: search.NodeScanner{Index: index},
			Peers: scatterFunc(func(table []search.Assigned) []search.Slice {
				out := make([]search.Slice, 0, len(table))
				for _, a := range table {
					reply := peer
					reply.Node, reply.Shards = a.Node, a.Shards
					out = append(out, reply)
				}
				return out
			}),
			Roster: func(context.Context) ([]string, error) {
				return []string{"node-a", "node-b"}, nil
			},
			// ABOVE THE FLOOR, so the search is divided between the two
			// nodes rather than answered by this one alone.
			Corpus: func(context.Context) (int, error) { return search.FanOutFloor, nil },
		}})
		got, err := items.Search(t.Context(), "signing key", 0)
		if err != nil {
			t.Fatalf("search the work items: %v", err)
		}
		return got
	}

	building := ask(search.Slice{Building: true})
	if p := building.Partial; p == nil || p.BucketsMissing == 0 ||
		!slices.Equal(p.AbsentNodes, []string{"node-b"}) ||
		p.BucketsAnswered+p.BucketsMissing != search.SearchShards {
		t.Fatalf("a search a building peer did not cover carries %+v — want the "+
			"peer's buckets missing and the peer named", building.Partial)
	}
	for _, item := range building.Items {
		if item.Key != "ENG-1" {
			t.Errorf("the covered half answered %+v", building.Items)
		}
	}

	answered := ask(search.Slice{})
	if answered.Partial != nil {
		t.Errorf("a search every participant covered carries %+v", answered.Partial)
	}
}

// scatterFunc is a peer set answering whatever the function returns for the
// table it was handed.
type scatterFunc func([]search.Assigned) []search.Slice

func (f scatterFunc) Scatter(_ context.Context, _ search.FanQuery,
	table []search.Assigned) ([]search.Slice, error) {
	return f(table), nil
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
