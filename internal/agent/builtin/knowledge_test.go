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
	"github.com/crewlet/crewlet/internal/tools"
)

// stubSearcher is a knowledge backend with a scripted answer and a record of
// what it was asked.
type stubSearcher struct {
	can     bool
	hits    []knowledge.Hit
	queries []knowledge.Query

	// building reports this node's index as still on its first build. A
	// backend that keeps no index answers false, which is the zero value.
	building bool
}

func (s *stubSearcher) CanSearch(*org.Role, *org.Organization) bool { return s.can }

func (s *stubSearcher) Building(context.Context) bool { return s.building }

func (s *stubSearcher) Search(_ context.Context, q knowledge.Query) []knowledge.Hit {
	s.queries = append(s.queries, q)
	return s.hits
}

// A NODE STILL INDEXING SAYS SO, whatever its search found.
//
// Empty, it answers in the turn-start block's own sentence: "no documents
// match" invites different keywords, which on an index still on its first build
// find nothing either, and a seat that concludes nothing was written down acts
// on it by writing a page that already exists.
//
// NOT EMPTY, it keeps what it found and says the answer may be missing pages:
// where the fleet divides a search, a node still building counts its own
// buckets missing and drops any page a peer ranked that it has not indexed
// yet, so the hits are real and the list is not whole.
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

	building := &stubSearcher{can: true, building: true}
	if got := ask(t, building); got != prefetch.BuildingKnowledgeHint {
		t.Errorf("an empty search on a building index answered %q, want the "+
			"turn-start block's building hint", got)
	}
	if len(building.queries) != 1 {
		t.Errorf("the backend saw %d searches, want the one that came back empty",
			len(building.queries))
	}

	// A SEARCH THAT FOUND SOMETHING KEEPS IT, and says it may not be all.
	found := &stubSearcher{can: true, building: true,
		hits: []knowledge.Hit{{Title: "Key rotation", Container: "ENG", PageID: "p-1"}}}
	got := ask(t, found)
	if !strings.Contains(got, "Key rotation") {
		t.Errorf("a building node discarded the hits it did find: %q", got)
	}
	if !strings.Contains(got, partialKnowledgeNote) {
		t.Errorf("a building node answered its hits as the whole answer: %q", got)
	}

	// THE CONTROLS: a built index, found or not, answers as itself.
	if got := ask(t, &stubSearcher{can: true}); !strings.Contains(got,
		"not everything is written down") {
		t.Errorf("an empty search on a built index answered %q, want the "+
			"no-match answer", got)
	}
	whole := &stubSearcher{can: true,
		hits: []knowledge.Hit{{Title: "Key rotation", Container: "ENG", PageID: "p-1"}}}
	if got := ask(t, whole); strings.Contains(got, partialKnowledgeNote) {
		t.Errorf("a built index's answer carries the building caveat: %q", got)
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
	// acting on the first two hundred characters of a runbook. So every
	// hit carries what opens it — its container and its page id, which the
	// page-read tool takes — and the answer ends saying how.
	backend := &stubSearcher{can: true, hits: []knowledge.Hit{
		{Title: "Staging runbook", Container: "ENG", PageID: "p-17",
			Snippet: "how the proxy is wired"},
		{Title: "Untitled page", Container: "OPS", PageID: "p-18"},
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
		prefetch.KnowledgeBullet(backend.hits[0]),
		prefetch.KnowledgeBullet(backend.hits[1]),
		"ENG, page id p-17", "OPS, page id p-18",
	} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("the answer is missing %q:\n%s", want, res.Output)
		}
	}
	if !strings.HasSuffix(res.Output, prefetch.KnowledgeReadHint) {
		t.Errorf("the answer does not end saying how to open a page:\n%s", res.Output)
	}
	// THE TOP OF A RANKING, NOT A COUNT OF WHAT MATCHED: "2 documents
	// match" tells a seat the company has two pages on the subject.
	if strings.Contains(res.Output, "documents match") {
		t.Errorf("the answer reports its cut as the number of matches:\n%s", res.Output)
	}
	if backend.queries[0].Limit != SearchKnowledgeHits {
		t.Errorf("the tool asked for %d pages, want %d", backend.queries[0].Limit,
			SearchKnowledgeHits)
	}
}

// THE DESCRIPTION NAMES THE NATIVE READER, AND WHAT IT TAKES.
//
// On the engine's own knowledge base a hit is opened with `get_page`, which
// takes the page id every hit renders; a description that sent a native seat
// to "your knowledge-base MCP tools" would send it looking for a server it does
// not have. A vendor wiki's reader is that vendor's own tool, described by what it
// does rather than by a name this engine does not own.
func TestSearchKnowledgeDescribesTheReaderByWhatItTakes(t *testing.T) {
	t.Parallel()
	desc := (&searchKnowledge{}).Description()
	if !strings.Contains(desc, "`"+GetPageTool+"`") {
		t.Errorf("the description does not name %s, the native backend's "+
			"reader: %s", GetPageTool, desc)
	}
	if !strings.Contains(desc, "page id") {
		t.Errorf("the description does not say what opens a hit: %s", desc)
	}
	if strings.Contains(desc, "MCP") {
		t.Errorf("the description sends a seat to MCP tools, which the native "+
			"backend's reader is not: %s", desc)
	}
	// AND WHAT A HIT CARRIES IS WHAT get_page TAKES: the id, beside the
	// container it lives in.
	bullet := prefetch.KnowledgeBullet(knowledge.Hit{Title: "Deploy runbook",
		Container: "ENG", PageID: "p-17"})
	if !strings.Contains(bullet, "ENG, page id p-17") {
		t.Errorf("a hit renders %q, which does not carry the page id get_page "+
			"takes", bullet)
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

// THE CHEAP GATE FIRST. A search that could not hit anything is told so,
// rather than waiting on a round trip that was always going to be empty — and
// the message names both causes it can be, because "no backend" and "no scope"
// send an operator to different places. The same sentence answers a seat and
// an operator's own assistant, which has no seat, so it never says "this
// seat".
func TestAnUnsearchableSeatIsToldSoWithoutASearch(t *testing.T) {
	t.Parallel()
	for name, call := range map[string]func(*searchKnowledge) (tools.Result, error){
		"a seat": func(tool *searchKnowledge) (tools.Result, error) {
			return tool.CallForTurn(context.Background(), searchTurn(),
				map[string]any{"query": "anything"})
		},
		"an operator": func(tool *searchKnowledge) (tools.Result, error) {
			tool.org = func() *org.Organization { return searchTurn().Org }
			return tool.Call(context.Background(), map[string]any{"query": "anything"})
		},
	} {
		backend := &stubSearcher{can: false}
		res, err := call(&searchKnowledge{search: backend})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(backend.queries) != 0 {
			t.Errorf("%s: a search ran behind a closed gate", name)
		}
		for _, want := range []string{"not searchable", "no knowledge backend",
			"`knowledge.scope`"} {
			if !strings.Contains(res.Output, want) {
				t.Errorf("%s: the answer does not say %q: %s", name, want, res.Output)
			}
		}
		if strings.Contains(res.Output, "this seat") {
			t.Errorf("%s: the answer speaks of a seat: %s", name, res.Output)
		}
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
// A cut would search on the first four hundred bytes of whatever was pasted in
// and answer with real, ranked pages about it — a plausible answer to a
// question the model never asked, and one it has no way to spot, because the
// hits look exactly like hits for the query it sent.
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
