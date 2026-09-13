package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Ranking work items over the node's own index.
//
// ONE INDEX, TWO CORPORA, TWO VERBS. The lexical index covers pages and work
// items together — see [search.LexicalSource] — and which of them a search
// answers from is a filter on the fan-out rather than a second index. So this
// is the same scan the knowledge search makes, with `task` where that one puts
// `page`, and the two rankings cannot drift because there is only one.

// itemRanker is [tracker.Ranker] over this node's index and fan-out.
type itemRanker struct {
	fan   *search.FanOut
	index *search.Indexer
}

// RankItems implements [tracker.Ranker].
func (r itemRanker) RankItems(ctx context.Context, text string,
	limit int) ([]tracker.RankedDoc, error) {

	if r.fan == nil || r.index == nil {
		return nil, nil
	}
	answer, err := r.fan.Search(ctx, search.FanQuery{
		Text:    text,
		Sources: []string{string(search.SourceTask)},
		Limit:   limit,
	})
	if err != nil {
		return nil, err
	}
	// PARTIAL IS NOT REFUSED, for the reason the knowledge search gives:
	// an answer over part of the corpus beats none. What differs here is
	// that the caller is a TOOL rather than a prompt block, so the fact
	// travels in the log rather than being swallowed — a short result set
	// is indistinguishable from a short corpus.
	if answer.Partial() {
		log.WarnContext(ctx, "work_search_scoped",
			"buckets_answered", answer.BucketsAnswered,
			"buckets_missing", answer.BucketsMissing,
			"detail", "the ranking was complete for what was searched and "+
				"silent about what was not")
	}
	hits, err := r.index.Hydrate(ctx, answer.Hits, text)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.RankedDoc, 0, len(hits))
	for _, hit := range hits {
		out = append(out, tracker.RankedDoc{ID: hit.ID, Snippet: hit.Snippet})
	}
	return out, nil
}

// Building implements [tracker.Ranker].
func (r itemRanker) Building(_ context.Context) bool {
	return r.index != nil && !r.index.Ready()
}

// WorkSearch is this node's ranked item search, or nil when it has no index —
// a company on another tracker, or a node whose native backends are off.
func (e *Engine) WorkSearch() *tracker.Searcher {
	n := e.native
	if n == nil || n.itemSearch == nil {
		return nil
	}
	return n.itemSearch
}

// WorkSearcher is [Engine.WorkSearch] as the tool layer's seam — the shape
// both the seat tools and a surface assembled outside this package take, the
// operator's MCP endpoint being the one that is.
//
// IT ANSWERS AN UNTYPED NIL for a node with no index, rather than a non-nil
// interface holding a nil [tracker.Searcher], because that is the shape a
// `deps.Search != nil` gate reads correctly: handed the other one it registers
// a search tool that can only fail.
//
// ONE FUNCTION FOR BOTH CALLERS. It was two — an unexported one here and a
// one-line exported wrapper around it — which is two places for the typed-nil
// rule above to be stated and one of them to stop matching.
func WorkSearcher(e *Engine) builtin.WorkSearcher {
	if s := e.WorkSearch(); s != nil {
		return s
	}
	return nil
}

// indexedCorpora is which corpora a node with these two backends indexes, and
// it is what the startup actually builds the index over.
//
// A FUNCTION rather than three lines inside the startup, because the defect it
// replaces could not be reached from a test without standing a whole company
// up: the index was built inside the block gated on the WIKI's backend, so a
// company with `tracker.backend: native` and Confluence for its knowledge
// indexed none of its own work items and served no ranked item search — while
// the embedding duty, armed on EITHER backend, went on paying for a vector on
// every one of them.
//
// IT FOLLOWS THE BACKENDS rather than covering both unconditionally: a company
// on Jira has no `tracker_tasks` worth walking, and a walk over a table its
// backend never fills is a cursor cycling over nothing on every tick.
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
