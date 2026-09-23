package pages_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
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
	// THE BACKSTOP'S OWN CASE: a draft at the top of its container has an
	// empty chain, which the seam cannot tell from one that did not come
	// back, so its title is what hides it.
	prefixed := r.write(author("jane"), pages.NewPage{
		Title: knowledge.AutoDraftTitlePrefix + "Rotating keys", Body: words,
	})
	reviewed := r.write(author("jane"), pages.NewPage{
		Title: "Signing key rotation", Body: words,
	})
	// AND THE GESTURE THAT MEANS REVIEWED: a draft moved out from under the
	// parent to another page, keeping its prefix. Its chain comes back and
	// carries no excluded title, so the prefix — the backstop for a chain
	// that did not — is not consulted, and it is returned.
	runbooks := r.write(author("jane"), pages.NewPage{
		Title: "Runbooks", Body: "an index of procedures",
	})
	moved := r.write(author("jane"), pages.NewPage{
		Title: knowledge.AutoDraftTitlePrefix + "Key rotation runbook", Body: words,
		ParentID: drafts.Page.ID,
	})
	if _, err := r.store.SavePage(t.Context(), author("jane"), moved.Page.ID, pages.Save{
		BaseVersion: moved.Page.Version, ParentID: &runbooks.Page.ID,
	}); err != nil {
		t.Fatalf("move the draft under Runbooks: %v", err)
	}
	r.drain()

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
	searcher, err := pages.NewSearcher(pages.SearcherOptions{Index: index, DB: r.db})
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}

	hidden := searcher.Search(t.Context(), knowledge.Query{Text: words})
	if got := hitIDs(hidden); !sameMembers(got, []string{reviewed.Page.ID, moved.Page.ID}) {
		t.Errorf("the default search returned %v, want %q and the draft moved "+
			"under Runbooks — the other three are under the excluded parent "+
			"or at the top of the container carrying its prefix",
			hitTitles(hidden), reviewed.Page.Title)
	}

	shown := searcher.Search(t.Context(), knowledge.Query{
		Text: words, ExcludeAncestors: []string{},
	})
	want := []string{under.Page.ID, deeper.Page.ID, prefixed.Page.ID,
		reviewed.Page.ID, moved.Page.ID}
	if got := hitIDs(shown); !sameMembers(got, want) {
		t.Errorf("a search that asked for no exclusion returned %v, want all "+
			"five pages that match", hitTitles(shown))
	}
	for _, hit := range shown {
		if hit.PageID == deeper.Page.ID && !slices.Equal(hit.Ancestors,
			[]string{knowledge.AutoDraftedParent, under.Page.Title}) {
			t.Errorf("the deeper draft's chain is %v, want the draft parent "+
				"then Key rotation, outermost first", hit.Ancestors)
		}
		if hit.PageID == moved.Page.ID && !slices.Equal(hit.Ancestors,
			[]string{runbooks.Page.Title}) {
			t.Errorf("the moved draft's chain is %v, want Runbooks alone",
				hit.Ancestors)
		}
	}

	// AND A CHAIN THAT CANNOT BE READ IS AN EMPTY ANSWER, never the hits
	// without it: served with no chain, the two drafts under the parent
	// that carry no prefix would reach a seat's prompt. The replicated
	// estate is closed LAST, because every read above goes through it.
	if err := r.db.CloseReplicated(); err != nil {
		t.Fatalf("close the replicated estate: %v", err)
	}
	if got := searcher.Search(t.Context(), knowledge.Query{Text: words}); len(got) != 0 {
		t.Errorf("a search whose chains could not be read returned %v, want "+
			"nothing", hitTitles(got))
	}
}

// A SEARCHER MISSING ITS STORE OR ITS INDEX IS REFUSED, NOT BUILT.
//
// Neither absence fails anything on its own. With no store no hit carries its
// parent chain, so a page under the auto-draft parent is returned unless its
// title carries the prefix; with no index nothing is searched at all. A seat
// cannot tell either from a knowledge base with nothing to hide or nothing
// written down, so the constructor is the one place either can be caught.
func TestASearcherMissingItsStoreOrItsIndexIsRefused(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	index := search.NewIndexerOver(db, []search.LexicalSource{search.PageSource{}})

	for name, tc := range map[string]struct {
		opts  pages.SearcherOptions
		names string
	}{
		"no store": {pages.SearcherOptions{Index: index}, "SearcherOptions.DB"},
		"no index": {pages.SearcherOptions{DB: db}, "SearcherOptions.Index"},
	} {
		searcher, err := pages.NewSearcher(tc.opts)
		if err == nil || searcher != nil {
			t.Errorf("%s: built %v with error %v, want a refusal", name, searcher, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.names) {
			t.Errorf("%s: the refusal %q does not name %s", name, err, tc.names)
		}
	}
	// THE CONTROL: with both, it builds — so the refusals above are about
	// what was missing rather than a constructor that refuses everything.
	if _, err := pages.NewSearcher(pages.SearcherOptions{Index: index, DB: db}); err != nil {
		t.Errorf("a searcher with its store and its index was refused: %v", err)
	}
}

// A KNOWLEDGE SEARCH WAITS FOR THE PAGES' FIRST BUILD AND FOR NOTHING ELSE.
//
// The index can cover the work items as well as the pages, and each corpus
// finishes its first build on its own. The prefetch asks
// [pages.Searcher.Building] before it searches at all, so a searcher that
// asked about the whole index would decline every knowledge search on a node
// whose pages were built for as long as its work items were not. The second
// source here stands for that other corpus, and never finishes a lap.
func TestAKnowledgeSearchWaitsForThePagesFirstBuildAlone(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const words = "rotate the signing key"
	written := r.write(author("jane"), pages.NewPage{Title: "Key rotation", Body: words})

	index := search.NewIndexerOver(r.db,
		[]search.LexicalSource{search.PageSource{}, unfinishedSource{}})
	searcher, err := pages.NewSearcher(pages.SearcherOptions{Index: index, DB: r.db})
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}
	// THE CONTROL: before any lap it IS building, so the answer below is
	// the pages' lap finishing rather than a gate that never closes.
	if !searcher.Building(t.Context()) {
		t.Fatal("a searcher over an index that has built nothing says it is not building")
	}

	// The page corpus comes first, so its lap finishes before the sweep
	// reaches the source that never does — which then fails every sweep.
	for sweeps := 0; !index.ReadyFor(string(search.SourcePage)); sweeps++ {
		if sweeps == 100 {
			t.Fatal("the page corpus never finished its first lap")
		}
		if _, err := index.Sweep(t.Context()); err != nil && !errors.Is(err, errUnfinished) {
			t.Fatalf("index the pages: %v", err)
		}
	}
	if index.Ready() {
		t.Fatal("the unfinished corpus reports its first build done, so this " +
			"case cannot tell one corpus's gate from the whole index's")
	}

	if searcher.Building(t.Context()) {
		t.Error("the pages are built and the searcher still says it is building — " +
			"the prefetch would search none of them until another corpus finished")
	}
	got := searcher.Search(t.Context(), knowledge.Query{Text: words})
	if ids := hitIDs(got); !slices.Equal(ids, []string{written.Page.ID}) {
		t.Errorf("the search returned %v, want the one page", hitTitles(got))
	}
}

// errUnfinished is what [unfinishedSource] answers every scan with.
var errUnfinished = errors.New("this corpus has not finished its first lap")

// unfinishedSource is a corpus whose first lap never finishes: every scan of
// it fails, so the index never marks it built.
type unfinishedSource struct{}

func (unfinishedSource) Source() string { return "unfinished" }

func (unfinishedSource) Versions(context.Context, *sql.Tx, string, int) ([]search.DocVersion, error) {
	return nil, errUnfinished
}

func (unfinishedSource) Fetch(context.Context, *sql.Tx, []string) ([]search.Doc, error) {
	return nil, errUnfinished
}

func (unfinishedSource) Live(context.Context, *sql.Tx, []string) (map[string]bool, error) {
	return nil, errUnfinished
}

func (unfinishedSource) Count(context.Context, *sql.Tx) (int, error) {
	return 0, errUnfinished
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
