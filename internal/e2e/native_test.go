package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/work"
)

// The engine's own tracker and knowledge base, end to end.
//
// What these assert is NOT that a store round-trips — the unit suites cover
// that against fakes. It is that a REAL node, with a real embedded broker, a
// real store and the real API in front of them, writes a record to the
// fleet, projects it onto itself, and answers a screen's question from that
// projection. Every one of those is a wire, and a company on the default
// backends has no other way to record anything.

// operator writes as a person's own credential would.
func operator() work.Actor {
	return work.Actor{Kind: work.AuthorOperator, OperatorID: "e2e"}
}

func pageOperator() pages.Actor {
	return pages.Actor{Kind: pages.AuthorOperator, OperatorID: "e2e"}
}

// A COMPANY THAT CONFIGURES NOTHING GETS A TRACKER AND A WIKI. That is the
// default, and it is the case a quickstart hits: no `tracker` block, no
// `knowledge` block, no integrations at all.
func TestADefaultCompanyHasATrackerAndAWiki(t *testing.T) {
	t.Parallel()
	n := start(t)
	waitFor(t, "the native backends to hydrate", n.engine.NativeHydrated)

	if n.engine.WorkStore() == nil || n.engine.Work() == nil {
		t.Fatal("a company that declares no tracker got no native one")
	}
	if n.engine.PagesStore() == nil || n.engine.Pages() == nil {
		t.Fatal("a company that declares no knowledge base got no native one")
	}
	// AND THE SEARCHER IS THE NATIVE ONE, which is what the turn-start
	// prefetch and every seat's search_knowledge read through.
	searcher := n.engine.Knowledge()
	if searcher == nil {
		t.Fatal("a default company got no knowledge searcher")
	}
	if got := searcher.Backend(); got != "native" {
		t.Errorf("the default searcher answers for %q", got)
	}
}

// A WRITE REACHES THE FLEET'S RECORD AND THEN THIS NODE'S PROJECTION.
//
// The two are different estates and the second is what every read goes to, so
// a write that landed on one and not the other is a board that disagrees with
// the record — which is exactly the failure a projection has to be watched
// for rather than assumed.
func TestAnItemWrittenToTheFleetLandsOnTheBoard(t *testing.T) {
	t.Parallel()
	n := start(t)
	waitFor(t, "the native backends to hydrate", n.engine.NativeHydrated)

	written, err := n.engine.WorkStore().Create(t.Context(), operator(), work.NewItem{
		Project: "ENG", Type: work.TypeBug, Title: "the deploy hangs on rollback",
		Body: "reproduces on every second run", Assignee: "ceo",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(written.Item.Key, "ENG-") {
		t.Errorf("the item was keyed %q, not ENG-n", written.Item.Key)
	}

	// READ THROUGH THE PROJECTION, not through the store: this is what a
	// board, a tool and the REST route all use, and it is the copy that
	// can be behind.
	if err := n.engine.WaitApplied(t.Context(), coord.FamilyWork, written.Revision); err != nil {
		t.Fatalf("wait for the projection: %v", err)
	}
	detail, err := n.engine.Work().Get(t.Context(), written.Item.Key)
	if err != nil {
		t.Fatalf("read %s back: %v", written.Item.Key, err)
	}
	if detail.Item.Title != "the deploy hangs on rollback" {
		t.Errorf("the projected item reads %q", detail.Item.Title)
	}
	if detail.Item.Assignee != "ceo" {
		t.Errorf("the projected assignee is %q", detail.Item.Assignee)
	}

	// AND THE LISTING FINDS IT, which is a different query from the get:
	// a board filters, and a filter that reached no rows would draw an
	// empty board over a company that has work.
	items, err := n.engine.Work().List(t.Context(), work.Filter{Project: "ENG"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := slices.ContainsFunc(items, func(s work.Summary) bool {
		return s.Key == written.Item.Key
	})
	if !found {
		t.Errorf("the board lists %d items and not the one just filed", len(items))
	}
}

// A PAGE IS FINDABLE BY SEARCH, which is the whole reason the knowledge base
// keeps an index rather than only rows.
//
// The index is built BEHIND the projection, asynchronously — so this also
// asserts the one thing that makes that safe: a node still indexing reports
// itself as building rather than answering an empty search, because an empty
// answer is one a seat acts on by writing a page that already exists.
func TestAPageBecomesSearchable(t *testing.T) {
	t.Parallel()
	n := start(t)
	waitFor(t, "the native backends to hydrate", n.engine.NativeHydrated)

	written, err := n.engine.PagesStore().Create(t.Context(), pageOperator(), pages.NewPage{
		Container: "ENG", Title: "Rollback runbook",
		Body: "When a deploy hangs on rollback, drain the node before retrying.",
	})
	if err != nil {
		t.Fatalf("create page: %v", err)
	}
	if err := n.engine.WaitApplied(t.Context(), coord.FamilyPages, written.Revision); err != nil {
		t.Fatalf("wait for the projection: %v", err)
	}

	searcher := n.engine.NativeSearcher()
	if searcher == nil {
		t.Fatal("no native searcher")
	}
	// THE INDEX CATCHES UP ON ITS OWN. Until it does the searcher says so
	// — the assertion below is that the state ENDS, not that it never
	// happened.
	waitFor(t, "the index to catch up", func() bool {
		return !searcher.Building(t.Context())
	})

	hits := searcher.Search(t.Context(), knowledge.Query{
		Text: "rollback drain node", Org: n.engine.Company().Org, Limit: 5,
	})
	if len(hits) == 0 {
		t.Fatal("a page written a moment ago is not findable by its own words")
	}
	if hits[0].Title != "Rollback runbook" {
		t.Errorf("the top hit is %q", hits[0].Title)
	}
}

// THE OPERATOR'S OWN WRITES ARE ATTRIBUTED TO THE OPERATOR, not to a seat.
//
// It is what lets an audit tell a person's edit from an agent's, and it has to
// survive the round trip through the fleet record and the projection —
// dropping the actor kind anywhere in between would make every operator edit
// read as a colleague's.
func TestAnOperatorWriteIsStillAnOperatorWriteOnTheBoard(t *testing.T) {
	t.Parallel()
	n := start(t)
	waitFor(t, "the native backends to hydrate", n.engine.NativeHydrated)

	written, err := n.engine.WorkStore().Create(t.Context(), operator(), work.NewItem{
		Project: "OPS", Type: work.TypeTask, Title: "rotate the signing key",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := n.engine.WaitApplied(t.Context(), coord.FamilyWork, written.Revision); err != nil {
		t.Fatalf("wait for the projection: %v", err)
	}
	detail, err := n.engine.Work().Get(t.Context(), written.Item.Key)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(detail.History) == 0 {
		t.Fatal("the item has no history, so nothing records who filed it")
	}
	change := detail.History[len(detail.History)-1]
	if change.ActorKind != work.AuthorOperator {
		t.Errorf("the filing is attributed as %q, not as an operator", change.ActorKind)
	}
	if change.OperatorID != "e2e" {
		t.Errorf("the record names the operator %q", change.OperatorID)
	}
	// AND THE RENDERED NAME IS NAMESPACED, so it cannot be mistaken for a
	// colleague: a seat handle is lowercase alphanumerics and hyphens and
	// can never contain a colon, so `operator:e2e` is unambiguous wherever
	// the two appear in one column.
	if change.Actor != "operator:e2e" {
		t.Errorf("the change renders its actor as %q, want the namespaced "+
			"operator form", change.Actor)
	}
	if !strings.Contains(change.Actor, ":") {
		t.Error("an operator's rendered name is indistinguishable from a handle")
	}
}
