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

// itemCorpus is the corpus a work search reads, and the one its building gate
// asks about. ONE CONSTANT FOR BOTH, so the gate cannot wait on a corpus the
// search never reads — the index finishes each corpus's first lap on its own,
// and a gate asking about the whole index would answer a work search "still
// building" for as long as the PAGES were.
const itemCorpus = string(search.SourceTask)

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
		Sources: []string{itemCorpus},
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

// Building implements [tracker.Ranker]: whether this node's index has yet to
// finish its first lap over the work items — see [itemCorpus] for why not the
// whole index.
func (r itemRanker) Building(_ context.Context) bool {
	return r.index != nil && !r.index.ReadyFor(itemCorpus)
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
// ONE FUNCTION FOR BOTH CALLERS, so the typed-nil rule above is stated in one
// place rather than in two that can stop matching.
func WorkSearcher(e *Engine) builtin.WorkSearcher {
	if s := e.WorkSearch(); s != nil {
		return s
	}
	return nil
}

// indexedCorpora is which corpora a node with these two backends indexes, and
// it is what the startup actually builds the index over.
//
// A FUNCTION rather than three lines inside the startup, so which corpora a
// node indexes can be tested without standing a whole company up. It follows
// EITHER native backend rather than the wiki's alone: a company with
// `tracker.backend: native` and Confluence for its knowledge still indexes its
// own work items and serves a ranked item search.
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
