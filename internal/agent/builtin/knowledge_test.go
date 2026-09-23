package builtin

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
)

// stubSearcher is a knowledge backend with a scripted answer and a record of
// what it was asked.
type stubSearcher struct {
	can     bool
	hits    []knowledge.Hit
	queries []knowledge.Query
}

func (s *stubSearcher) CanSearch(*org.Role, *org.Organization) bool { return s.can }

func (s *stubSearcher) Search(_ context.Context, q knowledge.Query) []knowledge.Hit {
	s.queries = append(s.queries, q)
	return s.hits
}

// indexingSearcher is a backend that keeps an index of its own, and says
// whether this node's is still on its first build.
type indexingSearcher struct {
	stubSearcher
	building bool
}

func (s *indexingSearcher) Building(context.Context) bool { return s.building }

// A NODE STILL INDEXING SAYS SO, in the turn-start block's own sentence.
//
// "No documents match" invites different keywords, which on an index still on
// its first build find nothing either, and a seat that concludes nothing was
// written down acts on it by writing a page that already exists. The turn-start
// block already tells the two apart; a seat's own search has to as well, or the
// seat that followed the block's advice and searched gets the other answer.
func TestASearchOnABuildingIndexSaysSo(t *testing.T) {
	t.Parallel()
	ask := func(t *testing.T, backend KnowledgeSearcher) string {
		t.Helper()
		tool := &searchKnowledge{search: backend}
		res, err := tool.CallForTurn(context.Background(), searchTurn(),
			map[string]any{"query": "signing key rotation"})
		if err != nil {
			t.Fatalf("CallForTurn: %v", err)
		}
		// NOT A FAILED CALL: nothing is wrong, and a failure would send
		// the model looking for a tool that works.
		if res.Failed {
			t.Fatalf("the search failed the call: %s", res.Output)
		}
		return res.Output
	}

	building := &indexingSearcher{stubSearcher: stubSearcher{can: true}, building: true}
	if got := ask(t, building); got != prefetch.BuildingKnowledgeHint {
		t.Errorf("an empty search on a building index answered %q, want the "+
			"turn-start block's building hint", got)
	}
	if len(building.queries) != 1 {
		t.Errorf("the backend saw %d searches, want the one that came back empty",
			len(building.queries))
	}

	// A SEARCH THAT FOUND SOMETHING HAS FOUND IT, whether or not this
	// node's own index is still catching up — a peer may have answered.
	found := &indexingSearcher{building: true, stubSearcher: stubSearcher{
		can: true, hits: []knowledge.Hit{{Title: "Key rotation"}},
	}}
	if got := ask(t, found); !strings.Contains(got, "Key rotation") {
		t.Errorf("a building node discarded the hits it did find: %q", got)
	}

	// THE CONTROLS: a built index, and a backend that keeps none, answer
	// an empty search as an empty search.
	for name, backend := range map[string]KnowledgeSearcher{
		"built":    &indexingSearcher{stubSearcher: stubSearcher{can: true}},
		"no index": &stubSearcher{can: true},
	} {
		if got := ask(t, backend); !strings.Contains(got, "not everything is written down") {
			t.Errorf("%s: an empty search answered %q, want the no-match answer", name, got)
		}
	}
}

func searchTurn() *turnctx.Turn {
	role := &org.Role{Name: "Engineer", DeclaredHandle: "eng"}
	return &turnctx.Turn{
		RunID: "run-1", WorkKey: "t-1", Seat: role,
		Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}},
	}
}

func TestSearchKnowledgeRendersPointersNotPages(t *testing.T) {
	t.Parallel()
	// THE POINTER IS THE POINT: a seat that acted on a snippet would be
	// acting on the first two hundred characters of a runbook.
	backend := &stubSearcher{can: true, hits: []knowledge.Hit{
		{Title: "Staging runbook", Snippet: "how the proxy is wired"},
		{Title: "Untitled page"},
	}}
	tool := &searchKnowledge{search: backend}
	res, err := tool.CallForTurn(context.Background(), searchTurn(),
		map[string]any{"query": "staging redirect proxy"})
	if err != nil {
		t.Fatalf("CallForTurn: %v", err)
	}
	if res.Failed {
		t.Fatalf("a real search failed: %s", res.Output)
	}
	for _, want := range []string{
		"Staging runbook", "how the proxy is wired", "Untitled page",
		"look it up by title",
	} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("the answer is missing %q:\n%s", want, res.Output)
		}
	}
}

// AUTO-DRAFTS ARE HIDDEN, the same exclusion the turn-start prefetch applies:
// an agent cannot tell an unreviewed proposal from a ratified runbook, and
// following one is how a draft becomes policy without anybody agreeing to it.
func TestSearchKnowledgeExcludesAutoDrafts(t *testing.T) {
	t.Parallel()
	backend := &stubSearcher{can: true, hits: []knowledge.Hit{{Title: "x"}}}
	tool := &searchKnowledge{search: backend}
	if _, err := tool.CallForTurn(context.Background(), searchTurn(),
		map[string]any{"query": "anything"}); err != nil {
		t.Fatalf("CallForTurn: %v", err)
	}
	if len(backend.queries) != 1 {
		t.Fatalf("the backend saw %d queries", len(backend.queries))
	}
	q := backend.queries[0]
	if len(q.ExcludeAncestors) != 1 || q.ExcludeAncestors[0] != knowledge.AutoDraftedParent {
		t.Errorf("exclusions = %v, want the auto-draft parent", q.ExcludeAncestors)
	}
	// And the SEAT is what the backend authenticates as, or one seat could
	// read what its own account never could.
	if q.Seat == nil || q.Seat.Handle() != "eng" {
		t.Errorf("the search did not carry the calling seat: %+v", q.Seat)
	}
}

// THE CHEAP GATE FIRST. A seat whose search could not hit anything is told
// so, rather than waiting on a round trip that was always going to be empty —
// and the message says which of the two states it is in, because "no backend"
// and "no scope" send an operator to different places.
func TestAnUnsearchableSeatIsToldSoWithoutASearch(t *testing.T) {
	t.Parallel()
	backend := &stubSearcher{can: false}
	tool := &searchKnowledge{search: backend}
	res, err := tool.CallForTurn(context.Background(), searchTurn(),
		map[string]any{"query": "anything"})
	if err != nil {
		t.Fatalf("CallForTurn: %v", err)
	}
	if len(backend.queries) != 0 {
		t.Error("a search ran behind a closed gate")
	}
	if !strings.Contains(res.Output, "not searchable") {
		t.Errorf("the answer does not say why: %s", res.Output)
	}
}

// AN EMPTY RESULT IS NOT A FAILURE. Not everything is written down, and a
// failed tool call would send the model looking for a tool that works.
func TestNoMatchesIsAnOrdinaryAnswer(t *testing.T) {
	t.Parallel()
	tool := &searchKnowledge{search: &stubSearcher{can: true}}
	res, _ := tool.CallForTurn(context.Background(), searchTurn(),
		map[string]any{"query": "nothing here"})
	if res.Failed {
		t.Errorf("an empty search failed the call: %s", res.Output)
	}
	if !strings.Contains(res.Output, "not everything is written down") {
		t.Errorf("the answer does not say what to do next: %s", res.Output)
	}
}

func TestSearchKnowledgeRefusesAnEmptyQuery(t *testing.T) {
	t.Parallel()
	tool := &searchKnowledge{search: &stubSearcher{can: true}}
	for _, args := range []map[string]any{{}, {"query": "   "}} {
		res, _ := tool.CallForTurn(context.Background(), searchTurn(), args)
		if !res.Failed {
			t.Errorf("%v was accepted", args)
		}
	}
}

// The scope and the credential are the SEAT's, so a call with no turn cannot
// search — and must say so rather than panicking: a tool surface built
// outside a turn is a real state (a validate command, a test).
func TestSearchKnowledgeWithNoTurnRefusesRatherThanPanicking(t *testing.T) {
	t.Parallel()
	tool := &searchKnowledge{search: &stubSearcher{can: true}}
	res, err := tool.Call(context.Background(), map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.Failed {
		t.Error("a seatless call searched anyway")
	}
}

// A LONG QUERY IS REFUSED, NEVER CUT, and NOTHING reaches the backend.
//
// The cut this replaced searched on the first four hundred bytes of whatever
// was pasted in and answered with real, ranked pages about it — a plausible
// answer to a question the model never asked, and one it had no way to spot,
// because the hits look exactly like hits for the query it sent.
func TestALongQueryIsRefusedRatherThanCut(t *testing.T) {
	t.Parallel()
	backend := &stubSearcher{can: true, hits: []knowledge.Hit{{Title: "x"}}}
	tool := &searchKnowledge{search: backend}
	res, err := tool.CallForTurn(context.Background(), searchTurn(),
		map[string]any{"query": strings.Repeat("z", 5000)})
	if err != nil {
		t.Fatalf("CallForTurn: %v", err)
	}
	if !res.Failed {
		t.Error("an over-long query was accepted")
	}
	if len(backend.queries) != 0 {
		t.Errorf("the backend was searched anyway, with %d bytes",
			len(backend.queries[0].Text))
	}
	// NAMES THE FIELD AND THE BOUND: a refusal the model cannot act on
	// costs the same round as a cut and buys nothing.
	for _, want := range []string{"`query`", "5000", strconv.Itoa(searchQueryMax)} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("the refusal does not mention %s: %q", want, res.Output)
		}
	}
}

// A query AT the bound is served: the refusal is for what exceeds it, and an
// off-by-one here would refuse the longest query the tool documents.
func TestAQueryAtTheBoundIsServed(t *testing.T) {
	t.Parallel()
	backend := &stubSearcher{can: true, hits: []knowledge.Hit{{Title: "x"}}}
	tool := &searchKnowledge{search: backend}
	query := strings.Repeat("z", searchQueryMax)
	res, err := tool.CallForTurn(context.Background(), searchTurn(),
		map[string]any{"query": query})
	if err != nil {
		t.Fatalf("CallForTurn: %v", err)
	}
	if res.Failed {
		t.Fatalf("a query at the bound was refused: %q", res.Output)
	}
	if len(backend.queries) != 1 || backend.queries[0].Text != query {
		t.Error("the query did not reach the backend verbatim")
	}
}
