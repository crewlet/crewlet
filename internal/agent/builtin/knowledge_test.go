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
	// refused is the gate's answer; the zero value lets every search run.
	refused knowledge.Refusal
	hits    []knowledge.Hit
	partial *knowledge.Partial
	failed  bool
	queries []knowledge.Query

	// truncated reports an answer the backend cut short of its ranking.
	truncated bool

	// building reports this node's index as still on its first build. A
	// backend that keeps no index answers false, which is the zero value.
	building bool
}

func (s *stubSearcher) CanSearch(*org.Role, *org.Organization) knowledge.Refusal {
	return s.refused
}

func (s *stubSearcher) Building(context.Context) bool { return s.building }

func (s *stubSearcher) Search(_ context.Context, q knowledge.Query) knowledge.Answer {
	s.queries = append(s.queries, q)
	return knowledge.Answer{Hits: s.hits, Partial: s.partial, Failed: s.failed, Truncated: s.truncated}
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

	building := &stubSearcher{building: true}
	if got := ask(t, building); got != prefetch.BuildingKnowledgeHint {
		t.Errorf("an empty search on a building index answered %q, want the "+
			"turn-start block's building hint", got)
	}
	if len(building.queries) != 1 {
		t.Errorf("the backend saw %d searches, want the one that came back empty",
			len(building.queries))
	}

	// A SEARCH THAT FOUND SOMETHING KEEPS IT, and says it may not be all.
	found := &stubSearcher{building: true,
		hits: []knowledge.Hit{{Title: "Key rotation", Container: "ENG", PageID: "p-1"}}}
	got := ask(t, found)
	if !strings.Contains(got, "Key rotation") {
		t.Errorf("a building node discarded the hits it did find: %q", got)
	}
	if !strings.Contains(got, partialKnowledgeNote) {
		t.Errorf("a building node answered its hits as the whole answer: %q", got)
	}

	// THE CONTROLS: a built index, found or not, answers as itself.
	if got := ask(t, &stubSearcher{}); !strings.Contains(got,
		"not everything is written down") {
		t.Errorf("an empty search on a built index answered %q, want the "+
			"no-match answer", got)
	}
	whole := &stubSearcher{
		hits: []knowledge.Hit{{Title: "Key rotation", Container: "ENG", PageID: "p-1"}}}
	if got := ask(t, whole); strings.Contains(got, partialKnowledgeNote) {
		t.Errorf("a built index's answer carries the building caveat: %q", got)
	}
}

// A TRUNCATED ANSWER SAYS SO, in the turn-start block's own sentence.
//
// A backend that read its ranking to a depth, and whose exclusions took places
// among what it read, answers fewer pages than it was asked for while it ranks
// more — and a seat reading the short list as everything that matched
// concludes a page it was not shown does not exist. Found or not, and on a
// node still indexing too, where the index's build explains a missing share
// but not a cut the backend made to its own ranking.
func TestATruncatedSearchSaysSo(t *testing.T) {
	t.Parallel()
	hit := []knowledge.Hit{{Title: "Key rotation", Container: "ENG", PageID: "p-1"}}
	for name, backend := range map[string]*stubSearcher{
		"found":                  {truncated: true, hits: hit},
		"found nothing":          {truncated: true},
		"found while indexing":   {truncated: true, building: true, hits: hit},
		"nothing while indexing": {truncated: true, building: true},
	} {
		res, err := (&searchKnowledge{search: backend}).CallForTurn(context.Background(), searchTurn(),
			map[string]any{"query": "signing key rotation"})
		if err != nil || res.Failed {
			t.Fatalf("%s: CallForTurn = %+v, %v", name, res, err)
		}
		if !strings.Contains(res.Output, prefetch.TruncatedKnowledgeNote) {
			t.Errorf("%s: the answer does not say it was cut short: %q", name, res.Output)
		}
	}
	res, err := (&searchKnowledge{search: &stubSearcher{hits: hit}}).CallForTurn(context.Background(),
		searchTurn(), map[string]any{"query": "signing key rotation"})
	if err != nil || strings.Contains(res.Output, prefetch.TruncatedKnowledge) {
		t.Errorf("a whole answer says it was cut short: %q (%v)", res.Output, err)
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
	backend := &stubSearcher{hits: []knowledge.Hit{
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
	backend := &stubSearcher{hits: []knowledge.Hit{{Title: "x"}}}
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

// THE CHEAP GATE FIRST, AND IT NAMES THE ONE STATE THAT IS TRUE.
//
// A search that could not hit anything is told so, rather than waiting on a
// round trip that was always going to be empty. There are three such states,
// and each sends whoever reads it somewhere different: a company with no
// knowledge base, one whose knowledge base this node is not serving — a
// Confluence org token that did not resolve, the engine's own on a node that
// did not start with it — and one with nothing this search may read. A
// sentence listing every cause names false ones, and sends a reader to check
// a setting that is already right, so each answer names its own state and no
// other's. The same sentence answers a seat and an operator's own assistant,
// which has no seat, so it never says "this seat".
func TestAnUnsearchableSearchNamesTheStateItIsIn(t *testing.T) {
	t.Parallel()
	causes := map[knowledge.Unsearchable]string{
		knowledge.NoBackend: "the company runs no knowledge base",
		knowledge.NotServed: "`integrations.confluence.token` resolves to no credential",
		knowledge.NoScope:   "no read scope (`knowledge.scope`) is declared",
	}
	callers := map[string]func(*searchKnowledge) (tools.Result, error){
		"a seat": func(tool *searchKnowledge) (tools.Result, error) {
			return tool.CallForTurn(context.Background(), searchTurn(),
				map[string]any{"query": "anything"})
		},
		"an operator": func(tool *searchKnowledge) (tools.Result, error) {
			tool.org = func() *org.Organization { return searchTurn().Org }
			return tool.Call(context.Background(), map[string]any{"query": "anything"})
		},
	}
	for state, cause := range causes {
		for name, call := range callers {
			backend := &stubSearcher{refused: knowledge.Refusal{State: state, Detail: cause}}
			res, err := call(&searchKnowledge{search: backend})
			if err != nil {
				t.Fatalf("%s, %s: %v", state, name, err)
			}
			if len(backend.queries) != 0 {
				t.Errorf("%s, %s: a search ran behind a closed gate", state, name)
			}
			if !strings.Contains(res.Output, "not searchable") ||
				!strings.Contains(res.Output, cause) {
				t.Errorf("%s, %s: the answer does not name its own cause %q: %s",
					state, name, cause, res.Output)
			}
			for other, elsewhere := range causes {
				if other != state && strings.Contains(res.Output, elsewhere) {
					t.Errorf("%s, %s: the answer names %s's cause as well: %s",
						state, name, other, res.Output)
				}
			}
			if strings.Contains(res.Output, "this seat") {
				t.Errorf("%s, %s: the answer speaks of a seat: %s", state, name, res.Output)
			}
		}
	}

	// A REFUSER THAT WROTE NO SENTENCE is still named by its state.
	tool := &searchKnowledge{search: &stubSearcher{
		refused: knowledge.Refusal{State: knowledge.NotServed}}}
	res, err := tool.CallForTurn(context.Background(), searchTurn(),
		map[string]any{"query": "anything"})
	if err != nil {
		t.Fatalf("CallForTurn: %v", err)
	}
	if !strings.Contains(res.Output, "this node is not serving") {
		t.Errorf("a refusal with no detail answered %q, which does not say the "+
			"knowledge base is not served here", res.Output)
	}
}

// A PARTIAL ANSWER SAYS WHAT IT IS MISSING, whatever it found.
//
// Where the fleet divides a search, a node that did not answer in time costs
// its share of the corpus, and a query the semantic half could not embed is
// answered on its words alone. Either way the hits are real and the list is
// not whole, and a seat reading it as whole concludes a page it did not see
// does not exist — so the answer carries the turn-start block's own sentence,
// found or empty. A whole answer carries none: a caveat on every answer is one
// nobody reads.
func TestAPartialAnswerSaysWhatItIsMissing(t *testing.T) {
	t.Parallel()
	partial := &knowledge.Partial{BucketsAnswered: 42, BucketsMissing: 22,
		AbsentNodes: []string{"node-b"}, SemanticSkipped: true}
	note := prefetch.PartialKnowledgeNote(partial)
	if note == "" {
		t.Fatal("the note for a partial answer is empty, so this case asserts nothing")
	}
	ask := func(backend *stubSearcher) string {
		t.Helper()
		res, err := (&searchKnowledge{search: backend}).CallForTurn(context.Background(),
			searchTurn(), map[string]any{"query": "signing key rotation"})
		if err != nil || res.Failed {
			t.Fatalf("CallForTurn: %v, %+v", err, res)
		}
		return res.Output
	}
	hit := []knowledge.Hit{{Title: "Key rotation", Container: "ENG", PageID: "p-1"}}
	if got := ask(&stubSearcher{hits: hit, partial: partial}); !strings.Contains(got, note) ||
		!strings.Contains(got, "Key rotation") {
		t.Errorf("a partial answer with a hit reads %q — want the hit and the note", got)
	}
	if got := ask(&stubSearcher{partial: partial}); !strings.Contains(got, note) {
		t.Errorf("an empty partial answer reads %q — \"nothing matched\" over part "+
			"of the corpus is not \"nothing matched\"", got)
	}
	if got := ask(&stubSearcher{hits: hit}); strings.Contains(got, "partial") {
		t.Errorf("a whole answer carries a partial caveat: %q", got)
	}
}

// A SEARCH THAT FAILED IS NOT ONE THAT MATCHED NOTHING. "No team documents
// match" sends a seat to try other words against a search that is not running,
// or to write the page it could not find; the failure says it failed, in the
// turn-start block's own sentence, and on a building node too — the search did
// not run there either.
func TestAFailedSearchSaysItFailedRatherThanThatNothingMatched(t *testing.T) {
	t.Parallel()
	for name, backend := range map[string]*stubSearcher{
		"a built index":    {failed: true},
		"a building index": {failed: true, building: true},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&searchKnowledge{search: backend}).CallForTurn(
				context.Background(), searchTurn(),
				map[string]any{"query": "signing key rotation"})
			if err != nil {
				t.Fatalf("CallForTurn: %v", err)
			}
			if !res.Failed || res.Output != prefetch.FailedKnowledgeHint {
				t.Errorf("a failed search answered %q (failed %v), want the "+
					"block's own sentence as a failed call", res.Output, res.Failed)
			}
		})
	}
}

// AN EMPTY RESULT IS NOT A FAILURE. Not everything is written down, and a
// failed tool call would send the model looking for a tool that works.
func TestNoMatchesIsAnOrdinaryAnswer(t *testing.T) {
	t.Parallel()
	tool := &searchKnowledge{search: &stubSearcher{}}
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
	tool := &searchKnowledge{search: &stubSearcher{}}
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
	tool := &searchKnowledge{search: &stubSearcher{}}
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
	// ONE BYTE PAST THE BOUND AS WELL AS FAR PAST IT: the first is what
	// pins the bound itself, since a query far past any plausible bound is
	// refused by all of them.
	for _, size := range []int{knowledge.MaxQueryBytes + 1, 5000} {
		backend := &stubSearcher{hits: []knowledge.Hit{{Title: "x"}}}
		tool := &searchKnowledge{search: backend}
		res, err := tool.CallForTurn(context.Background(), searchTurn(),
			map[string]any{"query": strings.Repeat("z", size)})
		if err != nil {
			t.Fatalf("%d bytes: CallForTurn: %v", size, err)
		}
		if !res.Failed {
			t.Errorf("a %d-byte query was accepted", size)
		}
		if len(backend.queries) != 0 {
			t.Errorf("%d bytes: the backend was searched anyway, with %d bytes",
				size, len(backend.queries[0].Text))
		}
		// NAMES THE FIELD AND THE BOUND: a refusal the model cannot act
		// on costs the same round as a cut and buys nothing.
		for _, want := range []string{"`query`", strconv.Itoa(size),
			strconv.Itoa(knowledge.MaxQueryBytes)} {
			if !strings.Contains(res.Output, want) {
				t.Errorf("%d bytes: the refusal does not mention %s: %q", size,
					want, res.Output)
			}
		}
	}
}

// A query AT the bound is served: the refusal is for what exceeds it, and an
// off-by-one here would refuse the longest query the tool documents.
func TestAQueryAtTheBoundIsServed(t *testing.T) {
	t.Parallel()
	backend := &stubSearcher{hits: []knowledge.Hit{{Title: "x"}}}
	tool := &searchKnowledge{search: backend}
	query := strings.Repeat("z", knowledge.MaxQueryBytes)
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
