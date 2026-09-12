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
		out = append(out, tracker.RankedDoc{
			ID: hit.ID, Snippet: hit.Snippet, Score: hit.Score,
		})
	}
	return out, nil
}

// Building implements [tracker.Ranker].
func (r itemRanker) Building(ctx context.Context) bool {
	if r.index == nil {
		return false
	}
	ready, err := r.index.Ready(ctx)
	if err != nil {
		// UNKNOWN READS AS BUILDING, for [pages.Searcher.Building]'s
		// reason: "still building" is harmless to hear and "nothing
		// written down" is something a seat acts on.
		return true
	}
	return !ready
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

// workSearchOrNil is [Engine.WorkSearch] as the tool layer's seam, and it
// answers a TYPED NIL for a node with no index rather than a non-nil
// interface holding one — which is the shape that makes a
// `deps.Search != nil` gate register a tool that can only fail.
func workSearchOrNil(e *Engine) builtin.WorkSearcher {
	if s := e.WorkSearch(); s != nil {
		return s
	}
	return nil
}
