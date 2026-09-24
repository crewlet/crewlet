package engine_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/engine"
)

// A LIVE MOVE BETWEEN KNOWLEDGE BACKENDS TAKES EFFECT AT THE APPLY, BOTH WAYS.
//
// The native knowledge base and tracker start with the node and keep running
// through a revision that moves the company off them, so a node can hold a
// native searcher and a Confluence one at once. What answers is the backend the
// CURRENT revision names: answered by which searcher exists, a company that
// moved to Confluence would go on searching its old native pages, and its seats
// would go on writing pages there that no search of the company reads. The
// native tools follow the same field, and so do the tracker's.
//
// And the other way: a node that booted on the native backends answers
// natively again the moment the company moves back — and a company on
// `backend: none` that keeps its Confluence block for routing is answered by
// nothing, while its page activity still routes.
func TestTheKnowledgeBaseFollowsALiveMoveBothWays(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	const confluenceBlock = `
  confluence:
    url: https://wiki.example.com
    token: t
    webhook_secret: cf`
	onVendors := companyDoc + `
integrations:` + confluenceBlock + `
  jira:
    url: https://jira.example.com
    token: t
    webhook_secret: js
`
	routingOnly := companyDoc + `
knowledge:
  backend: none
integrations:` + confluenceBlock + `
`
	apply := func(doc string) {
		t.Helper()
		if _, _, err := e.Apply(t.Context(), parsedCompany(t, doc)); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	// wantSearch is the backend expected to answer, or "" for none; pages and
	// work say whether the engine's own page tools and tracker tools are on
	// the epoch's surface.
	check := func(step, wantSearch string, pages, work bool) {
		t.Helper()
		got := e.Knowledge()
		switch {
		case wantSearch == "" && got != nil:
			t.Errorf("%s: %s answers the company's searches, want none", step, got.Backend())
		case wantSearch != "" && got == nil:
			t.Errorf("%s: nothing answers the company's searches, want %s", step, wantSearch)
		case wantSearch != "" && got.Backend() != wantSearch:
			t.Errorf("%s: %s answers the company's searches, want %s", step,
				got.Backend(), wantSearch)
		}
		for name, want := range map[string]bool{
			builtin.WritePageTool: pages, builtin.CreateWorkItemTool: work,
		} {
			if _, has := e.Company().Tools.Lookup(name); has != want {
				t.Errorf("%s: %s is registered = %v, want %v", step, name, has, want)
			}
		}
	}

	check("booted on the native backends", "native", true, true)
	apply(onVendors)
	check("moved to Confluence and Jira", "confluence", false, false)
	apply(companyDoc)
	check("moved back to the native backends", "native", true, true)
	apply(routingOnly)
	check("knowledge turned off, Confluence kept for routing", "", false, true)
	if !slices.Contains(e.RoutedSources(), "confluence") {
		t.Errorf("a company that kept its Confluence block for routing does not "+
			"route it: %v", e.RoutedSources())
	}
}
