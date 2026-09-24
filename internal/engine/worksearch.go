package engine

import (
	"context"
	"strings"

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

// itemRanker is [tracker.Ranker] over this node's index and fan-out, and it
// needs both: [Engine.startNative] builds it with both or not at all.
type itemRanker struct {
	fan   *search.FanOut
	index *search.Indexer
}

// RankItems implements [tracker.Ranker].
func (r itemRanker) RankItems(ctx context.Context, text string,
	limit int) ([]tracker.RankedDoc, *tracker.SearchPartial, error) {

	answer, err := r.fan.Search(ctx, search.FanQuery{
		Text:    text,
		Sources: []string{itemCorpus},
		Limit:   limit,
	})
	if err != nil {
		return nil, nil, err
	}
	// A PARTIAL ANSWER IS CARRIED, because a short result set is
	// indistinguishable from a short corpus to whoever reads it. The one
	// cause of it that this node can know in advance — its own index still
	// on its first build, which counts its own buckets missing — the
	// tracker's searcher refuses on through [itemRanker.Building]; what
	// remains is a peer that did not answer in time or answered that its
	// own index is still building, and a semantic half that did not run.
	partial := itemPartialOf(answer)
	if answer.Partial() {
		log.WarnContext(ctx, "work_search_scoped",
			"buckets_answered", answer.BucketsAnswered,
			"buckets_missing", answer.BucketsMissing,
			"absent", strings.Join(answer.Absent, ","),
			"detail", "the ranking carries what it did not search")
	}
	hits, err := r.index.Hydrate(ctx, answer.Hits, text)
	if err != nil {
		return nil, nil, err
	}
	out := make([]tracker.RankedDoc, 0, len(hits))
	for _, hit := range hits {
		out = append(out, tracker.RankedDoc{ID: hit.ID, Snippet: hit.Snippet})
	}
	return out, partial, nil
}

// itemPartialOf is what a fan-out answer is missing, on the tracker's terms, or
// nil for a whole one.
func itemPartialOf(a search.Answer) *tracker.SearchPartial {
	if a.Whole() {
		return nil
	}
	return &tracker.SearchPartial{
		BucketsAnswered: a.BucketsAnswered,
		BucketsMissing:  a.BucketsMissing,
		// NEVER NIL, so the wire reads `[]` rather than `null` for an
		// answer that is missing only its semantic half.
		AbsentNodes:     append([]string{}, a.Absent...),
		SemanticSkipped: a.SemanticSkipped,
	}
}

// Building implements [tracker.Ranker]: whether this node's index has yet to
// finish its first lap over the work items — see [itemCorpus] for why not the
// whole index.
func (r itemRanker) Building(_ context.Context) bool {
	return !r.index.ReadyFor(itemCorpus)
}

// WorkSearch is this node's ranked item search, or nil where this node does
// not run the engine's own tracker.
//
// ASKED OF THE TRACKER, NOT OF THE INDEX. The index is built under either
// native backend, so a company on another tracker that keeps its knowledge
// in the engine's own pages holds an index, and a searcher over it, with no
// work item in it: offered, that search answers every question with nothing,
// which a reader takes for "no such work" about work that lives in another
// tracker.
func (e *Engine) WorkSearch() *tracker.Searcher {
	n := e.native
	if n == nil || n.itemSearch == nil || n.trackerReader == nil {
		return nil
	}
	return n.itemSearch
}

// WorkSearcher is [Engine.WorkSearch] as the tool layer's seam — the shape
// both the seat tools and a surface assembled outside this package take, the
// operator's MCP endpoint being the one that is.
//
// IT ANSWERS AN UNTYPED NIL where [Engine.WorkSearch] answers nil, rather than
// a non-nil interface holding a nil [tracker.Searcher], because that is the
// shape a `deps.Search != nil` gate reads correctly: handed the other one it
// registers a search tool that can only fail.
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
