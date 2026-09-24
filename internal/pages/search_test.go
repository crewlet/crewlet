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
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
)

// A NATIVE SEARCH HONOURS THE ANCESTOR EXCLUSION, AT ANY DEPTH.
//
// [knowledge.Query.ExcludeAncestors] is part of the seam's contract, and its
// default is what keeps an unreviewed page under the auto-draft parent out of
// every seat's prompt. The turn-start prefetch and `search_knowledge` both set
// it to the auto-draft parent. The chain is read from the page rows, so a
// draft two levels down is hidden as surely as one directly under the parent —
// and a caller that passes an empty exclusion, deliberately, sees every one.
//
// THE TITLE PREFIX JUDGES ONLY A CHAIN THIS NODE COULD NOT READ WHOLE. Every
// hit's chain is read here, so a draft moved out from under the parent is
// returned wherever it landed — under another page or at the top of its
// container — because moving is the gesture that means reviewed. What the
// prefix hides is a draft whose chain runs into a parent this node no longer
// holds: what was above that parent is unknown, and an unknown chain fails
// closed.
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
	// A DRAFT AT THE TOP OF ITS CONTAINER, prefix kept. Its chain was read
	// and is empty, which is a page under nothing — so it is returned.
	prefixed := r.write(author("jane"), pages.NewPage{
		Title: knowledge.AutoDraftTitlePrefix + "Rotating keys", Body: words,
	})
	reviewed := r.write(author("jane"), pages.NewPage{
		Title: "Signing key rotation", Body: words,
	})
	// AND THE GESTURE THAT MEANS REVIEWED: a draft moved out from under the
	// parent to another page, keeping its prefix. Its chain comes back and
	// carries no excluded title, so the prefix is not consulted.
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
	// THE BACKSTOP'S OWN CASE: two pages under a parent that is then
	// purged, so each one's chain runs into a page this node no longer
	// holds. The draft is hidden by its title; the ordinary page is not,
	// because an unknown chain is no reason to hide a title that claims
	// nothing.
	holding := r.write(author("jane"), pages.NewPage{
		Title: "Holding pen", Body: "pages waiting for a home",
	})
	orphaned := r.write(author("jane"), pages.NewPage{
		Title: knowledge.AutoDraftTitlePrefix + "Signing keys", Body: words,
		ParentID: holding.Page.ID,
	})
	stray := r.write(author("jane"), pages.NewPage{
		Title: "Key ceremony notes", Body: words, ParentID: holding.Page.ID,
	})
	if _, err := r.store.Purge(t.Context(), author("jane"), holding.Page.ID,
		"emptied"); err != nil {
		t.Fatalf("purge the holding page: %v", err)
	}
	r.drain()

	index := search.NewIndexerOver(r.db, []search.LexicalSource{search.PageSource{}})
	indexUntilQuiet(t, index)
	searcher, err := pages.NewSearcher(pages.SearcherOptions{Index: index, DB: r.db})
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}

	hidden := searcher.Search(t.Context(), knowledge.Query{Text: words}).Hits
	if got := hitIDs(hidden); !sameMembers(got, []string{reviewed.Page.ID,
		prefixed.Page.ID, moved.Page.ID, stray.Page.ID}) {
		t.Errorf("the default search returned %v, want %q, the two drafts moved "+
			"out and the page whose parent was purged — the other three are "+
			"under the excluded parent or carry its prefix on a chain this "+
			"node could not read", hitTitles(hidden), reviewed.Page.Title)
	}

	shown := searcher.Search(t.Context(), knowledge.Query{
		Text: words, ExcludeAncestors: []string{},
	}).Hits
	want := []string{under.Page.ID, deeper.Page.ID, prefixed.Page.ID,
		reviewed.Page.ID, moved.Page.ID, orphaned.Page.ID, stray.Page.ID}
	if got := hitIDs(shown); !sameMembers(got, want) {
		t.Errorf("a search that asked for no exclusion returned %v, want all "+
			"seven pages that match", hitTitles(shown))
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
		// WHOLE WHERE THE WALK REACHED THE TOP, and not where it ran
		// into the purged parent.
		wantWhole := hit.PageID != orphaned.Page.ID && hit.PageID != stray.Page.ID
		if hit.AncestorsKnown != wantWhole {
			t.Errorf("%q reports its chain known=%v, want %v", hit.Title,
				hit.AncestorsKnown, wantWhole)
		}
	}

	// AND A CHAIN THAT CANNOT BE READ IS AN EMPTY ANSWER, never the hits
	// without it: served with no chain, the two drafts under the parent
	// that carry no prefix would reach a seat's prompt. The replicated
	// estate is closed LAST, because every read above goes through it.
	if err := r.db.CloseReplicated(); err != nil {
		t.Fatalf("close the replicated estate: %v", err)
	}
	if got := searcher.Search(t.Context(), knowledge.Query{Text: words}).Hits; len(got) != 0 {
		t.Errorf("a search whose chains could not be read returned %v, want "+
			"nothing", hitTitles(got))
	}
}

// A CHAIN THAT RUNS INTO A LOOP IS KNOWN WHOLE.
//
// Two moves of two pages, each decided before the other applied, can put each
// page under the other. The walk up either chain then stops where it meets a
// page it has already visited, having read every page on the loop — so nothing
// above it is unknown, and the exclusion judges it by the chain it read. The
// draft here is on such a loop under no excluded title, so it is returned with
// its prefix still on; judged unknown instead, the prefix would hide it.
func TestAChainThatLoopsIsKnownWhole(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const words = "rotate the signing key"
	draft := r.write(author("jane"), pages.NewPage{
		Title: knowledge.AutoDraftTitlePrefix + "Key rotation", Body: words,
	})
	runbooks := r.write(author("jane"), pages.NewPage{
		Title: "Runbooks", Body: "an index of procedures",
	})
	// NO DRAIN BETWEEN THEM, so each move is decided against rows where the
	// other page is still at the top, and neither is refused as a loop.
	if _, err := r.store.SavePage(t.Context(), author("jane"), draft.Page.ID,
		pages.Save{BaseVersion: draft.Page.Version, ParentID: &runbooks.Page.ID}); err != nil {
		t.Fatalf("put the draft under Runbooks: %v", err)
	}
	if _, err := r.store.SavePage(t.Context(), author("jane"), runbooks.Page.ID,
		pages.Save{BaseVersion: runbooks.Page.Version, ParentID: &draft.Page.ID}); err != nil {
		t.Fatalf("put Runbooks under the draft: %v", err)
	}
	r.drain()
	if got := r.get(runbooks.Page.ID).Page.ParentID; got != draft.Page.ID {
		t.Fatalf("Runbooks' parent is %q, want the draft — the loop this case is "+
			"about never formed", got)
	}

	index := search.NewIndexerOver(r.db, []search.LexicalSource{search.PageSource{}})
	indexUntilQuiet(t, index)
	searcher, err := pages.NewSearcher(pages.SearcherOptions{Index: index, DB: r.db})
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}
	hits := searcher.Search(t.Context(), knowledge.Query{Text: words}).Hits
	if ids := hitIDs(hits); !slices.Equal(ids, []string{draft.Page.ID}) {
		t.Fatalf("the search returned %v, want the draft on the loop — hidden "+
			"by its prefix, its chain was judged unknown", hitTitles(hits))
	}
	if !hits[0].AncestorsKnown ||
		!slices.Equal(hits[0].Ancestors, []string{runbooks.Page.Title}) {
		t.Errorf("the draft's chain is %v known=%v, want [Runbooks] known",
			hits[0].Ancestors, hits[0].AncestorsKnown)
	}
}

// A HIT THE INDEX HAS NOT DROPPED YET IS NOT AN ANSWER.
//
// The index is behind this node's own rows by design: a page trashed or purged
// a moment ago keeps its index row until the indexer's next orphan pass. The
// parent-chain read that follows the ranking is the later of the two reads, so
// it is what decides — a purged page is one nobody can open, and a trashed one
// is not what a knowledge search answers with.
func TestAHitTheIndexHasNotDroppedYetIsNotReturned(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const words = "rotate the signing key"
	kept := r.write(author("jane"), pages.NewPage{Title: "Key rotation", Body: words})
	trashed := r.write(author("jane"), pages.NewPage{Title: "Old key rotation", Body: words})
	purged := r.write(author("jane"), pages.NewPage{Title: "Older key rotation", Body: words})

	index := search.NewIndexerOver(r.db, []search.LexicalSource{search.PageSource{}})
	indexUntilQuiet(t, index)
	searcher, err := pages.NewSearcher(pages.SearcherOptions{Index: index, DB: r.db})
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}
	// THE CONTROL: all three are indexed and returned, so what goes missing
	// below went missing at the read rather than never being there.
	all := []string{kept.Page.ID, trashed.Page.ID, purged.Page.ID}
	if got := hitIDs(searcher.Search(t.Context(), knowledge.Query{Text: words}).Hits); !sameMembers(got, all) {
		t.Fatalf("before any removal the search returned %v, want all three", got)
	}

	if _, err := r.store.Trash(t.Context(), author("jane"), trashed.Page.ID); err != nil {
		t.Fatalf("trash: %v", err)
	}
	if _, err := r.store.Purge(t.Context(), author("jane"), purged.Page.ID, "a duplicate"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()
	// NO SWEEP: the index still holds both rows, which is the window.
	got := searcher.Search(t.Context(), knowledge.Query{Text: words}).Hits
	if ids := hitIDs(got); !slices.Equal(ids, []string{kept.Page.ID}) {
		t.Errorf("with the index not yet swept the search returned %v, want %q "+
			"alone — a trashed page and a purged one are not answers",
			hitTitles(got), kept.Page.Title)
	}
}

// indexUntilQuiet sweeps until a whole lap finds nothing to do.
func indexUntilQuiet(t *testing.T, index *search.Indexer) {
	t.Helper()
	for sweeps := 0; ; sweeps++ {
		if sweeps == 1000 {
			t.Fatal("the index never settled")
		}
		worked, err := index.Sweep(t.Context())
		if err != nil {
			t.Fatalf("index the pages: %v", err)
		}
		if !worked {
			return
		}
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
	// the pages' lap finishing rather than a gate that was open all along.
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
	if index.ReadyFor() {
		t.Fatal("the unfinished corpus reports its first build done, so this " +
			"case cannot tell one corpus's gate from the whole index's")
	}

	if searcher.Building(t.Context()) {
		t.Error("the pages are built and the searcher still says it is building — " +
			"the prefetch would search none of them until another corpus finished")
	}
	got := searcher.Search(t.Context(), knowledge.Query{Text: words}).Hits
	if ids := hitIDs(got); !slices.Equal(ids, []string{written.Page.ID}) {
		t.Errorf("the search returned %v, want the one page", hitTitles(got))
	}
}

// A NATIVE ANSWER CARRIES WHAT IT IS MISSING.
//
// Every reader of the seam renders [knowledge.Answer.Partial] — the turn-start
// block, `search_knowledge`, the dashboard — so the searcher is where a divided
// search's own count has to reach it. Two ways in: a participant whose index is
// still on its first lap replies that it covered nothing, which here is this
// node's own range and so every bucket; and a query the meaning half could not
// embed is answered on its words alone. The control is the same search once
// the index is built and the query embeds, which carries nothing.
func TestANativeAnswerCarriesWhatItIsMissing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const words = "rotate the signing key"
	written := r.write(author("jane"), pages.NewPage{Title: "Key rotation", Body: words})
	r.drain()

	index := search.NewIndexerOver(r.db, []search.LexicalSource{search.PageSource{}})
	embedder := embeddings.NewFake(16)
	var fails bool
	searcher, err := pages.NewSearcher(pages.SearcherOptions{
		Index: index, DB: r.db, Node: "node-a",
		Space: func() (search.QuerySpace, bool) {
			if fails {
				return search.QuerySpace{Embedder: failingEmbedder{embedder},
					Model: "fake"}, true
			}
			return search.QuerySpace{Embedder: embedder, Model: "fake"}, true
		},
	})
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}

	building := searcher.Search(t.Context(), knowledge.Query{Text: words})
	if p := building.Partial; p == nil || p.BucketsMissing != search.SearchShards ||
		!slices.Equal(p.AbsentNodes, []string{"node-a"}) {
		t.Fatalf("a search over an index on its first lap carries %+v — want every "+
			"bucket missing and this node named", building.Partial)
	}

	indexUntilQuiet(t, index)
	whole := searcher.Search(t.Context(), knowledge.Query{Text: words})
	if whole.Partial != nil {
		t.Errorf("a whole answer carries %+v", whole.Partial)
	}
	if ids := hitIDs(whole.Hits); !slices.Equal(ids, []string{written.Page.ID}) {
		t.Errorf("the search returned %v, want the one page", hitTitles(whole.Hits))
	}

	fails = true
	lexical := searcher.Search(t.Context(), knowledge.Query{Text: words})
	if p := lexical.Partial; p == nil || !p.SemanticSkipped || p.BucketsMissing != 0 ||
		p.AbsentNodes == nil {
		t.Errorf("a query that could not be embedded carries %+v — want the "+
			"meaning half named as skipped, every bucket answered, and an "+
			"empty list of absent nodes rather than none", lexical.Partial)
	}
	if ids := hitIDs(lexical.Hits); !slices.Equal(ids, []string{written.Page.ID}) {
		t.Errorf("the lexical answer returned %v, want the page its words match",
			hitTitles(lexical.Hits))
	}
}

// A NATIVE SEARCH THAT FAILS SAYS IT FAILED. The seam is best effort, so a
// failure is still an answer a caller can render — and what it must not
// render is "nothing matched", which sends a seat to try other words against a
// search that did not run, or to write the page it could not find. The
// failure here is the fan-out's own refusal of an ask past what one fan-out
// can answer; the control is the same search within it.
func TestANativeSearchThatFailsSaysSo(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const words = "rotate the signing key"
	written := r.write(author("jane"), pages.NewPage{Title: "Key rotation", Body: words})
	r.drain()
	index := search.NewIndexerOver(r.db, []search.LexicalSource{search.PageSource{}})
	indexUntilQuiet(t, index)
	searcher, err := pages.NewSearcher(pages.SearcherOptions{Index: index, DB: r.db})
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}

	failed := searcher.Search(t.Context(), knowledge.Query{Text: words, Limit: search.FuseN})
	if !failed.Failed || len(failed.Hits) != 0 {
		t.Errorf("a search the fan-out refused answered %+v, want an empty "+
			"answer marked failed", failed)
	}
	whole := searcher.Search(t.Context(), knowledge.Query{Text: words})
	if whole.Failed {
		t.Error("a search that ran is marked failed")
	}
	if ids := hitIDs(whole.Hits); !slices.Equal(ids, []string{written.Page.ID}) {
		t.Errorf("the control returned %v, want the one page", hitTitles(whole.Hits))
	}
}

// failingEmbedder is a provider that refuses every text.
type failingEmbedder struct{ embeddings.Embedder }

func (failingEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("the provider refused")
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
