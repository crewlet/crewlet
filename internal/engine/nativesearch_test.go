package engine_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
)

// THE ENGINE'S OWN KNOWLEDGE SEARCH READS EVERY HIT'S PARENT CHAIN, so a page
// under the auto-draft parent stays out of a seat's search.
//
// Through [engine.Engine.Knowledge], which is the searcher the turn-start
// prefetch and `search_knowledge` both read through, on an engine built by
// [engine.New]. The exclusion itself is internal/pages' and is
// tested there against a searcher that test builds for itself; what only this
// case can see is whether the engine hands that searcher the store the chains
// are read from. Without it every hit reaches the exclusion with no chain, and
// only a title carrying the auto-draft prefix is hidden — so the draft below,
// whose title carries none, would be in the answer.
func TestTheEnginesKnowledgeSearchReadsEachHitsParentChain(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	waitFor(t, "the native backends to hydrate", e.NativeHydrated)
	searcher := e.Knowledge()
	if searcher == nil {
		t.Fatal("the default company runs the native knowledge base and the " +
			"engine offers no searcher")
	}

	operator := pages.Actor{Kind: pages.AuthorOperator, OperatorID: "ops-1"}
	write := func(in pages.NewPage) pages.Page {
		t.Helper()
		got, err := e.PagesStore().Create(t.Context(), operator, in)
		if err != nil {
			t.Fatalf("create %q: %v", in.Title, err)
		}
		// APPLIED BEFORE THE NEXT WRITE, because a create names its parent
		// by a page this node's own rows must already hold.
		if err := e.WaitCommitted(t.Context(), got.Outcome.Position); err != nil {
			t.Fatalf("wait for %q to apply: %v", in.Title, err)
		}
		return got.Page
	}
	const words = "rotate the signing key"
	parent := write(pages.NewPage{
		Container: "ENG", Title: knowledge.AutoDraftedParent, Body: "where proposals wait",
	})
	draft := write(pages.NewPage{
		Container: "ENG", Title: "Key rotation", Body: words, ParentID: parent.ID,
	})
	reviewed := write(pages.NewPage{
		Container: "ENG", Title: "Signing key rotation", Body: words,
	})
	org := e.Company().Org

	// BOTH ARE SEARCHABLE FIRST, asked with the exclusion turned off: the
	// index is built behind the rows on its own loop, and a draft missing
	// from the answer because it was not indexed yet would pass the
	// assertion at the end without testing anything.
	var all []knowledge.Hit
	waitFor(t, "both pages to become searchable", func() bool {
		all = searcher.Search(t.Context(), knowledge.Query{
			Text: words, Org: org, ExcludeAncestors: []string{},
		})
		return len(all) == 2
	})
	for _, hit := range all {
		if hit.PageID == draft.ID &&
			!slices.Equal(hit.Ancestors, []string{knowledge.AutoDraftedParent}) {
			t.Errorf("the draft's hit carries the chain %v, want [%s] — the "+
				"engine's searcher is reading no parent chain", hit.Ancestors,
				knowledge.AutoDraftedParent)
		}
	}

	got := searcher.Search(t.Context(), knowledge.Query{Text: words, Org: org})
	var ids, titles []string
	for _, hit := range got {
		ids, titles = append(ids, hit.PageID), append(titles, hit.Title)
	}
	if !slices.Equal(ids, []string{reviewed.ID}) {
		t.Errorf("the default search returned %v, want %q alone — %q is under "+
			"%q, which every seat's search excludes", titles, reviewed.Title,
			draft.Title, knowledge.AutoDraftedParent)
	}
}
