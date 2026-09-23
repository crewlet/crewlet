package pages_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
)

// A NATIVE SEARCH HONOURS THE ANCESTOR EXCLUSION, AT ANY DEPTH.
//
// [knowledge.Query.ExcludeAncestors] is part of the seam's contract, and its
// default is what keeps an unreviewed page under the auto-draft parent out of
// every seat's prompt. The turn-start prefetch and `search_knowledge` both set
// it to the auto-draft parent, and a searcher that never read it returned the
// drafts it exists to hide. The chain is read from the page rows, so a draft
// two levels down is hidden as surely as one directly under the parent — and
// a caller that passes an empty exclusion, deliberately, sees every one.
func TestANativeSearchHonoursTheAncestorExclusion(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const words = "rotate the signing key"
	drafts := r.write(author("jane"), pages.NewPage{
		Title: knowledge.AutoDraftedParent, Body: "where proposals wait",
	})
	under := r.write(author("jane"), pages.NewPage{
		Title: "Key rotation", Body: words, ParentID: drafts.Page.ID,
	})
	deeper := r.write(author("jane"), pages.NewPage{
		Title: "Key rotation in staging", Body: words, ParentID: under.Page.ID,
	})
	// THE BACKSTOP'S OWN CASE: a draft at the top of its container, with no
	// chain to judge by, is hidden by its title.
	prefixed := r.write(author("jane"), pages.NewPage{
		Title: knowledge.AutoDraftTitlePrefix + "Rotating keys", Body: words,
	})
	reviewed := r.write(author("jane"), pages.NewPage{
		Title: "Signing key rotation", Body: words,
	})

	index := search.NewIndexerOver(r.db, []search.LexicalSource{search.PageSource{}})
	for {
		worked, err := index.Sweep(t.Context())
		if err != nil {
			t.Fatalf("index the pages: %v", err)
		}
		if !worked {
			break
		}
	}
	searcher := pages.NewSearcher(pages.SearcherOptions{Index: index, DB: r.db})

	hidden := searcher.Search(t.Context(), knowledge.Query{Text: words})
	if got := hitIDs(hidden); !slices.Equal(got, []string{reviewed.Page.ID}) {
		t.Errorf("the default search returned %v, want the reviewed page %s "+
			"alone — the three drafts are under the excluded parent or "+
			"carry its prefix", hitTitles(hidden), reviewed.Page.Title)
	}

	shown := searcher.Search(t.Context(), knowledge.Query{
		Text: words, ExcludeAncestors: []string{},
	})
	want := []string{under.Page.ID, deeper.Page.ID, prefixed.Page.ID, reviewed.Page.ID}
	if got := hitIDs(shown); !sameMembers(got, want) {
		t.Errorf("a search that asked for no exclusion returned %v, want all "+
			"four pages that match", hitTitles(shown))
	}
	for _, hit := range shown {
		if hit.PageID == deeper.Page.ID && !slices.Equal(hit.Ancestors,
			[]string{knowledge.AutoDraftedParent, under.Page.Title}) {
			t.Errorf("the deeper draft's chain is %v, want the draft parent "+
				"then Key rotation, outermost first", hit.Ancestors)
		}
	}

	// WITHOUT A STORE THERE IS NO CHAIN, and only the title backstop is
	// left — which is what SearcherOptions.DB says a nil store costs.
	chainless := pages.NewSearcher(pages.SearcherOptions{Index: index})
	got := hitIDs(chainless.Search(t.Context(), knowledge.Query{Text: words}))
	if !sameMembers(got, []string{under.Page.ID, deeper.Page.ID, reviewed.Page.ID}) {
		t.Errorf("a searcher with no store returned %v, want every match but "+
			"the prefixed one", got)
	}
}

func hitIDs(hits []knowledge.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.PageID)
	}
	return out
}

func hitTitles(hits []knowledge.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.Title)
	}
	return out
}

func sameMembers(got, want []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want)))
}
