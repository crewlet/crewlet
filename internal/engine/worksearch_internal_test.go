package engine

import (
	"slices"
	"testing"
)

// THE INDEX FOLLOWS BOTH BACKENDS, not the wiki's alone.
//
// The lexical index covers pages AND work items, and it used to be built
// inside the block gated on `knowledge.backend: native`. A company running the
// engine's own tracker with Confluence for its knowledge therefore indexed
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
