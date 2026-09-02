package builtin

import (
	"context"
	"strings"
	"testing"

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

func searchTurn() *turnctx.Turn {
	role := &org.Role{Name: "Engineer", DeclaredHandle: "eng"}
	return &turnctx.Turn{
		ID: "t-1", Seat: role,
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

// A pathological query would reach a backend's own query language. Four
// hundred characters is a long sentence and several keywords; a whole thread
// pasted in is prose no ranker can use.
func TestALongQueryIsBounded(t *testing.T) {
	t.Parallel()
	backend := &stubSearcher{can: true, hits: []knowledge.Hit{{Title: "x"}}}
	tool := &searchKnowledge{search: backend}
	if _, err := tool.CallForTurn(context.Background(), searchTurn(),
		map[string]any{"query": strings.Repeat("z", 5000)}); err != nil {
		t.Fatalf("CallForTurn: %v", err)
	}
	if got := len(backend.queries[0].Text); got != searchQueryMax {
		t.Errorf("the query reached the backend at %d characters, want %d", got, searchQueryMax)
	}
}
