package engine

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE INDEX FOLLOWS BOTH BACKENDS, not the wiki's alone.
//
// The lexical index covers pages AND work items, and it used to be built
// inside the block gated on `knowledge.backend: doclet`. A company running
// Worklet with Confluence for its knowledge therefore indexed
// none of its own work items and served no ranked item search — while the
// embedding duty, armed on EITHER backend, went on paying for a vector on
// every one of them. A paid index nobody could query.
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

// THE RANKED WORK SEARCH FOLLOWS THE TRACKER, not the index alone. The index is
// built under EITHER backend, so a data node of a company on Jira with
// Doclet for its knowledge holds an item searcher over an index with no task in
// it — and handing that searcher out answered the dashboard's `work_search`
// with a ranking of nothing on a company whose work lives in Jira, where every
// other Worklet question answers unknown, the operator's catalogue and
// the seats' tools offer no such search, and a node without `data` hands out
// none. "Nothing matched" is the one answer a person acts on by filing the
// duplicate.
func TestTheRankedWorkSearchFollowsTheTracker(t *testing.T) {
	t.Parallel()
	q := memory.NewBroker().Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	router, err := estate.NewRouter(estate.RouterOptions{
		Self: "data-self", Queue: q, Placement: twoDataNodes{}, Git: noGitNodes{},
		Session: estate.NewSession(), Envelopes: routerEnvelopes(t), Timing: estate.Production(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		native *native
		remote *remoteNative
		want   bool
	}{
		{"a data node on Worklet",
			&native{trackerReader: &worklet.Reader{}, itemSearch: &worklet.Searcher{}}, nil, true},
		{"a data node on Jira with Doclet",
			&native{pageReader: &doclet.Reader{}, itemSearch: &worklet.Searcher{}}, nil, false},
		{"a node without data on Worklet", nil, &remoteNative{tracker: true}, true},
		{"a node without data on Jira with Doclet",
			nil, &remoteNative{wiki: true}, false},
		{"a node running no native runtime", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Engine{router: router}
			if tc.native != nil {
				e.native.Store(tc.native)
			}
			if tc.remote != nil {
				e.remote.Store(tc.remote)
			}
			if got := WorkSearcher(e) != nil; got != tc.want {
				t.Errorf("the ranked work search handed out = %v, want %v", got, tc.want)
			}
		})
	}
}
