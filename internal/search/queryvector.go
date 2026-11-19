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

// THE QUERY'S OWN VECTOR: a ranking needs the query in the document vectors'
// space, which [Semantic] ranks against.
//
// Cached, because an embedding is a billed provider round trip and the same
// strings recur (the ⌘K palette as a person types, a seat's `search_knowledge`
// after the prefetch, a refetch); [QueryEmbeddingCacheEntries] bounds it per node.
//
// Keyed on the MODEL and cleared when it moves: a vector is a point in ONE
// model's space, and after `providers.embeddings.model` changes an old-model
// vector would match, as [Semantic] filters by model, only rows not yet
// re-embedded, a ranking that silently shrinks during the refill. The cache
// belongs to one (model, width); a config apply with the SAME model keeps it.

// QueryEmbeddingCacheEntries bounds the per-node cache of query vectors.
//
// 1 024, which at the widest shipped width (3 072 float32s, 12 KiB packed) is
// ≈ 12 MiB — a bounded, small fraction of what a node holds for its page
// cache — and is far more distinct queries than a company asks in the minutes
// a string stays hot. Past it the least recently used entry goes.
const QueryEmbeddingCacheEntries = 1024

// QueryEmbedBudget bounds computing one query's vector.
//
// TWO SECONDS, against the provider client's ceilings (fifteen for one input,
// [embeddings.EmbedTimeout]; a minute per batch request,
// [embeddings.BatchTimeout]), because this caller sits in front of a person
// typing or a turn starting. It is twice [SemanticScanBudget]: past it the
// meaning half has cost two whole scans before it began, and the search serves
// the words (hybrid) or says so (semantic). A short input to a hosted endpoint
// answers in a few hundred milliseconds at the p99, so a healthy provider never
// meets it, nor does the one batch request of a long query ([queryEmbedding]),
// at most three times [knowledge.MaxQueryBytes] as sent (each stray byte
// becomes a U+FFFD, [embeddings.Prepare]).
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
	raw, err := queryEmbedding(bounded, provider, key)
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

// queryEmbedding is the vector of the WHOLE query.
//
// NEVER ITS OPENING: a source embeds as its opening because that says what it
// is about, but a vector of a question's first part ranks the answers to
// another. A query too long for the model is embedded whole, in pieces at the
// model's bound in one request, pooled ([embeddings.EmbedWhole]).
//
// ONLY A NARROW MODEL NEEDS THAT: surfaces refuse a query past
// [knowledge.MaxQueryBytes] and every documented model takes five times that in
// one input (it grows to three times as sent, [embeddings.Prepare]). A local
// model with a smaller `max_input_tokens` would refuse longer queries, each
// reading as the provider failing. A non-batching embedder keeps its bound.
func queryEmbedding(ctx context.Context, provider embeddings.Embedder, text string) ([]float32, error) {
	if batch, ok := provider.(embeddings.BatchEmbedder); ok {
		return embeddings.EmbedWhole(ctx, batch, text)
	}
	return provider.Embed(ctx, text)
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
