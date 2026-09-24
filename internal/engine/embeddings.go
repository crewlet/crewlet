package engine

import (
	"context"
	"fmt"
	"strings"

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

// vectorBackend is one epoch's embedding backend: the provider, the model id
// it embeds under, and whether the company's search uses it.
//
// ONE VALUE, CARRIED BY THE EPOCH ([Company]), because the three are only
// meaningful together. A vector is comparable only with vectors of its own
// model, so the id the embedding duty files rows under and the id a search
// selects them by must be the model the provider actually asks for — and the
// switch that says whether the company's search has a semantic half must be
// the same revision's. Held anywhere but on the epoch, one of them can move
// without the others: a provider stored for a revision that an apply then
// refused would embed under the served revision's model id.
type vectorBackend struct {
	// embedder is the provider. Never nil: a company with no provider has
	// no backend at all.
	embedder embeddings.Embedder

	// model is the model id the provider was built with, resolved in the
	// same read of the resolver as its credentials.
	model string

	// search is `knowledge.vectors` as this revision resolves it: whether
	// the engine's own knowledge and work-item search has a semantic half.
	// The diary and episode recall use the provider either way.
	search bool
}

// buildEmbedder constructs an epoch's embedding backend, or nil.
//
// PER EPOCH like every other provider, because an apply can change the model
// — but NOT the width: the store was sized at open and a revision that moved
// the width is refused here rather than allowed to write rows the reader
// cannot match. That check belongs at the apply, where an operator is
// watching, not at the first recall weeks later.
//
// ONE READ OF THE RESOLVER for the model id and every credential, so the id
// the backend carries is the model its provider asks for: resolved in two
// reads, a secret snapshot installed between them would pair one model's
// provider with another model's id.
func (e *Engine) buildEmbedder(c *Company) (*vectorBackend, error) {
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
	env := e.resolver()
	model := env.Value(cfg.Model)
	provider, err := embeddings.New(embeddings.Config{
		Model:      model,
		Dimensions: cfg.Width(),
		APIKey:     strings.TrimSpace(env.Value(cfg.APIKey)),
		BaseURL:    env.Value(cfg.BaseURL),
		LookupEnv:  env.Lookup,
	})
	if err != nil {
		return nil, err
	}
	return &vectorBackend{
		embedder: provider, model: model, search: c.Config.VectorsEnabled(),
	}, nil
}

// storeWidth is the width this node's store was opened at, or 0.
func (e *Engine) storeWidth() int {
	if e.backends == nil || e.backends.Store == nil {
		return 0
	}
	return e.backends.Store.EmbeddingDim()
}

// embed is the epoch's embedder as the prefetch and the learning stores take
// it, or nil.
//
// A FUNCTION rather than the interface, because that is what their seams ask
// for — and because it is where the one rule the callers share lives: an
// error is no vector, never a failure to propagate. Every consumer of a
// vector here is ranking, and a ranking that could not be computed costs
// relevance rather than correctness.
//
// NIL-SAFE on both counts — no epoch, and an epoch with no backend — because
// nil is how every consumer learns there is no similarity search, and a
// method value closing over a nil interface is not nil and panics on its
// first call.
func (c *Company) embed() func(context.Context, string) ([]float32, error) {
	if c == nil || c.vectors == nil {
		return nil
	}
	return c.vectors.embedder.Embed
}
