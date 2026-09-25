package search

import (
	"container/list"
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// THE QUERY'S OWN VECTOR, which is the one piece the semantic half was
// missing.
//
// The embedding duty fills a vector per DOCUMENT, the vector domain replicates
// them, and [Semantic] ranks against them — but a ranking needs the query in
// the same space, and until this existed nothing computed one: both callers of
// the fan-out passed text, sources and a limit, so every search skipped its
// semantic slice and answered lexical-only while the corpus was embedded, paid
// for and replicated to every node.
//
// # Why it is cached, and how much
//
// A query embedding is a provider round trip — a network call, billed —
// before a single row is scanned, and the same strings recur: the ⌘K palette
// asks as a person types, a seat's `search_knowledge` asks what the prefetch
// asked a moment earlier, a screen refetches. [QueryEmbeddingCacheEntries] is
// the bound, per node.
//
// # Why the cache is keyed on the MODEL and cleared when it moves
//
// A vector is a point in ONE model's space. After an operator changes
// `providers.embeddings.model`, a cached vector from the old model ranks
// against rows the duty is re-embedding in the new one — [Semantic] filters
// by model, so it would match only the rows not yet re-embedded, which is a
// ranking that silently shrinks as the refill progresses. So the cache
// belongs to one (model, width) and the first lookup under another empties it.
// A config apply that replaces the provider with the SAME model keeps it,
// because the vectors are the same vectors.

// QueryEmbeddingCacheEntries bounds the per-node cache of query vectors.
//
// 1 024, which at the widest shipped width (3 072 float32s, 12 KiB packed) is
// ≈ 12 MiB — a bounded, small fraction of what a node holds for its page
// cache — and is far more distinct queries than a company asks in the minutes
// a string stays hot. Past it the least recently used entry goes.
const QueryEmbeddingCacheEntries = 1024

// QueryEmbedBudget bounds computing one query's vector.
//
// TWO SECONDS, against the provider client's own fifteen, because the two
// guard different callers. The client's bound is the embedding DUTY's — a
// batch of 128 documents off the interactive path — while this one sits in
// front of a person typing or a turn starting. It is twice
// [SemanticScanBudget]: past it the meaning half has cost two whole scans
// before it began, and the search serves the words (hybrid) or says so
// (semantic) rather than hold the reader. A single short input to a hosted
// embeddings endpoint answers in a few hundred milliseconds at the p99, so a
// healthy provider never meets it.
const QueryEmbedBudget = 2 * time.Second

// EmbeddingModel is the company's embedder as a query needs it: the provider,
// the model id its vectors are stored under, and whether semantic search is
// on at all.
//
// A FUNCTION, READ PER QUERY, because the provider is replaced on every
// config apply and `knowledge.vectors` is Tier B: a value captured at start
// would embed with a retired model for as long as the process ran.
type EmbeddingModel func() (provider embeddings.Embedder, model string, on bool)

// QueryVector is one query's embedding, packed as [SemanticQuery.Vector].
type QueryVector struct {
	Vector []byte
	Model  string
	Dim    int
}

// QueryVectors computes and caches query vectors for one node.
type QueryVectors struct {
	model EmbeddingModel

	mu      sync.Mutex
	space   string // model id the entries were computed under
	dim     int
	order   *list.List // front is most recently used
	entries map[string]*list.Element
}

type cachedVector struct {
	text   string
	vector []byte
}

// NewQueryVectors builds the cache over the company's embedder. A nil model
// is a node with no semantic search at all.
func NewQueryVectors(model EmbeddingModel) *QueryVectors {
	return &QueryVectors{
		model:   model,
		order:   list.New(),
		entries: make(map[string]*list.Element),
	}
}

// Available reports, with NO I/O, whether a semantic ranking could run: a
// provider is configured and semantic search is on. It is what a backend
// answers [knowledge.Outcome.Modes] from.
func (v *QueryVectors) Available() bool {
	if v == nil || v.model == nil {
		return false
	}
	provider, model, on := v.model()
	return on && provider != nil && model != "" && provider.Width() > 0
}

// Vector is the query's embedding, or the degradation that says why there is
// none.
func (v *QueryVectors) Vector(ctx context.Context, text string) (QueryVector, knowledge.Degradation) {
	if v == nil || v.model == nil {
		return QueryVector{}, knowledge.DegradedNoEmbeddings
	}
	provider, model, on := v.model()
	if !on || provider == nil || model == "" || provider.Width() <= 0 {
		return QueryVector{}, knowledge.DegradedNoEmbeddings
	}
	dim := provider.Width()
	key := strings.TrimSpace(text)
	if key == "" {
		return QueryVector{}, knowledge.DegradedEmbeddingFailed
	}
	if packed, hit := v.lookup(model, dim, key); hit {
		return QueryVector{Vector: packed, Model: model, Dim: dim}, knowledge.NotDegraded
	}

	bounded, cancel := context.WithTimeout(ctx, QueryEmbedBudget)
	defer cancel()
	raw, err := provider.Embed(bounded, key)
	if err != nil {
		if !errors.Is(err, embeddings.ErrEmpty) {
			log.WarnContext(ctx, "search_query_embedding_failed",
				"model", model, "error", err.Error(),
				"detail", "a hybrid search serves its keyword half and a "+
					"semantic one answers nothing; the next search asks again")
		}
		return QueryVector{}, knowledge.DegradedEmbeddingFailed
	}
	packed, err := pack(raw, dim)
	if err != nil {
		// THE SAME REFUSAL THE DUTY MAKES, for a sharper reason here: a
		// non-finite query component scores every row as a perfect
		// match, so the ranking would be the table's order.
		log.WarnContext(ctx, "search_query_embedding_failed",
			"model", model, "error", err.Error())
		return QueryVector{}, knowledge.DegradedEmbeddingFailed
	}
	v.store(model, dim, key, packed)
	return QueryVector{Vector: packed, Model: model, Dim: dim}, knowledge.NotDegraded
}

// lookup answers a cached vector, emptying the cache first when the model or
// width it was filled under is no longer the company's.
func (v *QueryVectors) lookup(model string, dim int, key string) ([]byte, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.rebase(model, dim)
	el, ok := v.entries[key]
	if !ok {
		return nil, false
	}
	v.order.MoveToFront(el)
	return el.Value.(*cachedVector).vector, true
}

// store records a computed vector, evicting the least recently used entry
// past the bound.
func (v *QueryVectors) store(model string, dim int, key string, packed []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	// RE-CHECKED UNDER THE LOCK, because the provider call ran without
	// it: an apply that moved the model while this query was being
	// embedded must not file an old-space vector into the new space — nor
	// empty the new space's entries to make room for it. The vector was
	// still right for the search that asked; it is simply not kept.
	if v.space != model || v.dim != dim {
		return
	}
	if el, ok := v.entries[key]; ok {
		v.order.MoveToFront(el)
		return
	}
	v.entries[key] = v.order.PushFront(&cachedVector{text: key, vector: packed})
	for v.order.Len() > QueryEmbeddingCacheEntries {
		oldest := v.order.Back()
		v.order.Remove(oldest)
		delete(v.entries, oldest.Value.(*cachedVector).text)
	}
}

// rebase empties the cache when the embedding space moved. Held under mu.
func (v *QueryVectors) rebase(model string, dim int) {
	if v.space == model && v.dim == dim {
		return
	}
	v.space, v.dim = model, dim
	v.order.Init()
	clear(v.entries)
}

// Len is how many vectors are cached, for a test.
func (v *QueryVectors) Len() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.order.Len()
}
