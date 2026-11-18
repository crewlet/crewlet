package engine

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Ranking work items over the node's own index. The lexical index covers pages
// and work items together ([search.LexicalSource]) and the corpus is a filter
// on the fan-out, so this is the knowledge search's scan with `task` for
// `page`, and the two rankings cannot drift.

// itemRanker is [worklet.Ranker] over this node's index and fan-out.
type itemRanker struct {
	fan   *search.FanOut
	index *search.Indexer
}

// Candidates implements [worklet.Ranker]: each method the query's mode runs,
// its top candidates over this node's corpus of work items, the item behind
// each with the index's excerpt, and what the ranking did.
//
// PARTIAL IS NOT REFUSED, as in the knowledge search: an answer over part of
// the corpus beats none. The coverage rides in the answer's outcome to the
// surface that asked, where a person or a seat can act on it.
func (r itemRanker) Candidates(ctx context.Context, q worklet.SearchQuery) (worklet.RankedCandidates, error) {
	if r.fan == nil || r.index == nil {
		return worklet.RankedCandidates{}, nil
	}
	if strings.TrimSpace(q.Text) == "" {
		// THE PROBE: nothing runs or is read, but the answer says which modes this
		// node serves and how the asked one would degrade, so a screen can offer
		// the modes honestly before anybody types.
		return worklet.RankedCandidates{Outcome: knowledge.Outcome{
			Modes:    r.fan.Modes(),
			Degraded: r.fan.ProbeDegradation(q.Mode),
			Coverage: knowledge.Coverage{Nodes: []knowledge.NodeCoverage{}},
		}}, nil
	}
	answer, err := r.fan.Search(ctx, search.FanQuery{
		Text:    q.Text,
		Sources: []string{string(search.SourceTask)},
		Mode:    q.Mode,
		// NOT THE CALLER'S LIMIT: this answers each method's top FuseN
		// candidates; the fan-out's fused cut is not read here.
		Limit: search.FuseN,
	})
	if err != nil {
		return worklet.RankedCandidates{}, err
	}
	out := worklet.RankedCandidates{Candidates: answer.Candidates, Outcome: answer.Outcome()}
	keys := answer.Candidates.Keys()
	if len(keys) == 0 {
		return out, nil
	}
	hits, err := r.index.Hydrate(ctx, keys, q.Text)
	if err != nil {
		return worklet.RankedCandidates{}, err
	}
	out.Docs = make(map[string]worklet.RankedDoc, len(hits))
	for _, hit := range hits {
		out.Docs[hit.Key] = worklet.RankedDoc{ID: hit.ID, Snippet: hit.Snippet}
	}
	return out, nil
}

// Building implements [worklet.Ranker].
func (r itemRanker) Building(_ context.Context) bool {
	return r.index != nil && !r.index.Ready()
}

// WorkSearch is this node's ranked item search, or nil where there is no
// Worklet to search (a company whose tracker is a vendor's or none, or a node
// that has not met its company yet). The index is built wherever Worklet or
// Doclet runs, so Worklet decides the nil, not the index.
func (e *Engine) WorkSearch() *worklet.Searcher {
	n := e.native.Load()
	if n == nil || n.itemSearch == nil {
		return nil
	}
	return n.itemSearch
}

// WorkSearcher is [Engine.WorkSearch] as the tool layer's seam, taken by the
// seat tools and by surfaces outside this package (the operator's MCP
// endpoint). It answers through the router on every node, so a node holding no
// data asks one that does, the same verb over the same fleet-wide index.
//
// IT ANSWERS AN UNTYPED NIL, not an interface holding a nil [worklet.Searcher]:
// a `deps.Search != nil` gate handed the latter registers a tool that can only
// fail. And ONLY WHERE THE COMPANY RUNS WORKLET: the index is built under
// either backend ([indexedCorpora]), so under a vendor's tracker a searcher
// ranks nothing and reads as "nothing matched". [OperatorNative] decides that
// once for every operator read seam.
func WorkSearcher(e *Engine) builtin.WorkSearcher {
	if reads := OperatorNative(e); reads.Search {
		return e.router.Work()
	}
	return nil
}

// indexedCorpora is which corpora a node with these two backends indexes, and
// what the startup builds the index over: a function so
// TestTheIndexFollowsEitherNativeBackend reaches it without a whole company.
// It follows the tracker's backend as well as the wiki's, and no further: a
// company on Jira has no `tracker_tasks` worth walking, and a walk over a
// table its backend never fills cycles a cursor over nothing on every tick.
//
// CHAT IS NEVER ONE OF THEM: a conversation has an index of its own
// ([search.ChatIndexer]) with a forward-only walk and maintained statistics
// (ADR-0071), so a room's chatter never moves the BM25 statistics a page or a
// work item is ranked by.
func indexedCorpora(runTracker, wiki bool) []string {
	var out []string
	if runTracker {
		out = append(out, string(search.SourceTask))
	}
	if wiki {
		out = append(out, string(search.SourcePage))
	}
	return out
}

// lexicalSources is [indexedCorpora] as the indexer's own seam.
func lexicalSources(runTracker, wiki bool) []search.LexicalSource {
	byName := map[string]search.LexicalSource{
		string(search.SourceTask): search.TaskSource{},
		string(search.SourcePage): search.PageSource{},
	}
	var out []search.LexicalSource
	for _, name := range indexedCorpora(runTracker, wiki) {
		out = append(out, byName[name])
	}
	return out
}
