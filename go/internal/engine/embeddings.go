package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// The vector backend, wired.
//
// # It is optional, and every consumer already knows that
//
// A company with no providers.embeddings has no similarity search: the
// diary's candidate pool falls back to recency alone and episode recall
// renders nothing. Both are first-class states in the prefetch rather than
// failures — recent memories are still this seat's memories, while "similar
// prior work" with no similarity behind it would be a claim the block cannot
// support.
//
// So this returns nil freely, and the one thing it does NOT do is invent a
// default: the store's vector columns are sized from the configured width at
// open time, and an embedder nobody asked for would write rows at whatever
// width its default model happens to produce.

// buildEmbedder constructs the company's embedder, or nil.
//
// PER EPOCH like every other provider, because an apply can change the model
// — but NOT the width: the store was sized at open and a revision that moved
// the width is refused here rather than allowed to write rows the reader
// cannot match. That check belongs at the apply, where an operator is
// watching, not at the first recall weeks later.
func (e *Engine) buildEmbedder(c *Company) (embeddings.Embedder, error) {
	cfg := c.Config.Providers.Embeddings
	if cfg == nil {
		return nil, nil
	}
	if opened := e.storeWidth(); opened > 0 && cfg.Width() != opened {
		return nil, fmt.Errorf("engine: providers.embeddings.dimensions is %d "+
			"but this node's store was opened at %d; the vector columns are "+
			"sized at open and an apply cannot resize them — restart the node "+
			"to change the width", cfg.Width(), opened)
	}
	env := config.EnvOnly()
	provider, err := embeddings.New(embeddings.Config{
		Model:      env.Value(cfg.Model),
		Dimensions: cfg.Width(),
		APIKey:     strings.TrimSpace(env.Value(cfg.APIKey)),
		BaseURL:    env.Value(cfg.BaseURL),
		LookupEnv:  env.Lookup,
	})
	if err != nil {
		return nil, err
	}
	return provider, nil
}

// storeWidth is the width this node's store was opened at, or 0.
func (e *Engine) storeWidth() int {
	if e.backends == nil || e.backends.Store == nil {
		return 0
	}
	return e.backends.Store.EmbeddingDim()
}

// embedder is the company's embedder as the prefetch takes it, or nil.
//
// A FUNCTION rather than the interface, because that is what the prefetch's
// seam asks for — and because it is where the one rule the callers share
// lives: an error is no vector, never a failure to propagate. Every consumer
// of a vector here is ranking, and a ranking that could not be computed
// costs relevance rather than correctness.
func (e *Engine) embedder() func(context.Context, string) ([]float32, error) {
	embed := e.embeddings.Load()
	if embed == nil || *embed == nil {
		return nil
	}
	return (*embed).Embed
}
