package engine

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY SEARCH SOURCE NAMES THE DOMAIN ITS ROWS COME FROM, in both directions:
// a source this build indexes that maps to no domain is one no verdict covers,
// so a participant whose copy of it is wrong would scan it anyway; and a
// mapping to a name no domain is registered under asks a verdict that is never
// computed.
func TestEverySearchSourceNamesTheDomainItsRowsComeFrom(t *testing.T) {
	t.Parallel()
	if got, want := slices.Sorted(maps.Keys(sourceDomains)), slices.Sorted(slices.Values(search.Sources)); !slices.Equal(got, want) {
		t.Fatalf("sourceDomains maps %v, and this build indexes %v", got, want)
	}
	for src, domain := range sourceDomains {
		if !slices.Contains(registeredNames(), domain) {
			t.Errorf("%s's rows are mapped to %q, which no registered domain is named", src, domain)
		}
	}
}

// sliceOf asks the node, over the broker and through the answerer the engine
// registers for its peers, for its slice of one query over every bucket — the
// path a coordinator elsewhere in the fleet takes.
func sliceOf(t *testing.T, e *Engine, q search.FanQuery) search.Slice {
	t.Helper()
	slices, err := search.Broker{Queue: e.backends.Queue}.Scatter(t.Context(), q,
		[]search.Assigned{{Node: e.id, Shards: search.Everything()}})
	if err != nil || len(slices) != 1 {
		t.Fatalf("ask the node for its slice = (%d slices, %v)", len(slices), err)
	}
	return slices[0]
}

// semanticQuery is a query that runs both halves, over a 4-wide space no
// vector this node holds is in — so the semantic half, run, finds nothing and
// fails nothing.
func semanticQuery(sources ...string) search.FanQuery {
	return search.FanQuery{Text: "anything", Sources: sources,
		Vector: make([]byte, 16), Model: "probe", Dim: 4,
		Methods: []search.Method{search.MethodLexical, search.MethodSemantic}}
}

// indexed waits for the node's index to finish its first lap over both
// corpora, so a slice is answered rather than reported building.
func indexed(t *testing.T, e *Engine) {
	t.Helper()
	n := e.native.Load()
	waitUntil(t, 20*time.Second, "the index's first lap", func() bool {
		return n.indexer.ReadyFor(string(search.SourcePage), string(search.SourceTask))
	})
}

// A WRONG VECTORS COPY GIVES UP THE SEMANTIC HALF OF EVERY SLICE THE NODE
// SERVES ITS PEERS, and says why, on a real node through the answerer the
// engine registers (ADR-0072): a range's lexical half is still covered from
// copies that serve, and its semantic half is never read from vectors the node
// has stopped serving. Before, the slice a peer asked for read them with no
// verdict at all.
func TestAWrongVectorsCopyGivesUpTheSemanticHalfOfEverySlice(t *testing.T) {
	t.Parallel()
	e, _, _ := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	indexed(t, e)
	if got := sliceOf(t, e, semanticQuery()); got.SemanticSkipped || got.OutOfService {
		t.Fatalf("the premise: a sound node's slice of a semantic query = %+v", got)
	}

	wrongCopyOf(t, e, vectorsName)
	got := sliceOf(t, e, semanticQuery())
	switch {
	case got.OutOfService || got.Building:
		t.Fatalf("a wrong vectors copy left the range uncovered: %+v", got)
	case !got.SemanticSkipped || !got.VectorsOutOfService:
		t.Fatalf("a slice over a wrong vectors copy = %+v — its semantic half was read "+
			"from vectors the node stopped serving", got)
	}
}

// A WRONG KNOWLEDGE-BASE COPY COVERS NONE OF A KNOWLEDGE SEARCH, NOR OF A PEER'S
// SLICE OVER THE PAGES, on a real node through the searcher and the answerer
// the engine wires (ADR-0072) — and a slice over the tasks alone, whose copy
// is sound, is answered. Before, both read the index over rows the node had
// stopped serving and reported the range covered.
func TestAWrongPagesCopyCoversNoneOfAKnowledgeSearch(t *testing.T) {
	t.Parallel()
	e, _, _ := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	indexed(t, e)
	wrongCopyOf(t, e, pagesName)

	result, err := e.DocletSearcher().Answer(t.Context(),
		knowledge.Query{Text: "anything", Mode: knowledge.ModeKeyword})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	coverage := result.Outcome.Coverage
	if coverage.Complete || coverage.BucketsMissing != search.SearchShards {
		t.Fatalf("a knowledge search over a pages copy out of service covered %+v — its "+
			"range answered from rows the node stopped serving", coverage)
	}
	var named bool
	for _, node := range coverage.Nodes {
		named = named || (!node.Answered && strings.Contains(node.Error, "out of service"))
	}
	if !named {
		t.Fatalf("the coverage %+v does not name the copy out of service", coverage.Nodes)
	}

	lexical := []search.Method{search.MethodLexical}
	pagesSlice := sliceOf(t, e, search.FanQuery{Text: "anything",
		Sources: []string{string(search.SourcePage)}, Methods: lexical})
	if !pagesSlice.OutOfService {
		t.Fatalf("a peer's slice over a pages copy out of service = %+v, want nothing "+
			"covered, said so", pagesSlice)
	}
	tasksSlice := sliceOf(t, e, search.FanQuery{Text: "anything",
		Sources: []string{string(search.SourceTask)}, Methods: lexical})
	if tasksSlice.OutOfService || tasksSlice.Building {
		t.Fatalf("a peer's slice over the tasks alone = %+v — refused over the pages' copy",
			tasksSlice)
	}
}

// A NODE WHOSE TRACKER COPY IS WRONG COVERS NONE OF ITS SEARCH RANGE, on a
// real node, through the scanner the engine wires (ADR-0072).
//
// Before, the item search's own scanner — and every slice it served its peers —
// read the index over rows the node had stopped serving, and the coordinator
// merged that answer under complete coverage. Now the range is counted
// missing, named for this node, with the reason its copy gave.
func TestAWrongTrackerCopyCoversNoneOfItsSearchRange(t *testing.T) {
	t.Parallel()
	e, _, q := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	n := e.native.Load()
	waitUntil(t, 20*time.Second, "the index's first lap", func() bool {
		return n.indexer.ReadyFor(string(search.SourceTask))
	})
	s := e.core.Load().log
	s.rejoinMu.Lock()
	s.rejoin = func(context.Context) error { return nil }
	s.rejoinMu.Unlock()
	pushBelowTheFloor(t, e, q)

	e.local.mu.Lock()
	e.local.slot = verdictSlot{}
	e.local.mu.Unlock()
	waitUntil(t, 5*time.Second, "the tracker copy to go out of service", func() bool {
		_, serves := e.local.For(t.Context(), trackerName)
		return !serves
	})
	answer, err := n.itemSearch.Search(t.Context(), worklet.SearchQuery{
		Text: "anything", Mode: knowledge.ModeKeyword,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if answer.Coverage.Complete || answer.Coverage.BucketsMissing != search.SearchShards {
		t.Fatalf("a search over a tracker copy out of service covered %+v — its range "+
			"answered from rows the node stopped serving", answer.Coverage)
	}
	var named bool
	for _, node := range answer.Coverage.Nodes {
		named = named || (!node.Answered && strings.Contains(node.Error, "out of service"))
	}
	if !named {
		t.Fatalf("the coverage %+v does not name the copy out of service", answer.Coverage.Nodes)
	}
}
