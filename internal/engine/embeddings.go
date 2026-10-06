package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/learning"
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
	env := e.resolver()
	// THE MODEL'S LIMITS, as the configuration resolves them from what its
	// vendor documents and what the operator stated: the provider refuses
	// an input past them before sending it, and packs a batch into requests
	// they admit.
	limits := cfg.Limits()
	provider, err := embeddings.New(embeddings.Config{
		Model:      env.Value(cfg.Model),
		Dimensions: cfg.Width(),
		// THE CONFIGURATION'S RULE for which key, the conventional
		// OPENAI_API_KEY included, resolved through the store-aware chain.
		APIKey:  cfg.ResolvedKey(env),
		BaseURL: env.Value(cfg.BaseURL),
		Limits: embeddings.Limits{
			InputBytes:    limits.InputBytes,
			BatchInputs:   limits.BatchInputs,
			BatchBytes:    limits.BatchBytes,
			InputOverhead: limits.InputOverhead,
		},
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

// embedText is the company's embedder as the learning paths take it — the
// turn-start prefetch, the pull tools that re-run it, and the episodist: the
// [learning.Embed] seam, handed out as this method value.
//
// READ AT CALL TIME, never captured, and that is the whole reason it is a
// method rather than a function built over whatever [Engine.embeddings]
// held. An apply equips its epoch's tools BEFORE it stores the embedder it
// is applying (see [Engine.equip]), so a seam captured at build ran one
// epoch behind for its whole life: query_episodes answered "no embeddings"
// for the entire first epoch a node booted, and re-activating an unchanged
// revision to rotate the provider's key — the documented gesture — left the
// pull tools on the previous provider and its retired key. A company with
// none configured answers [learning.ErrNoEmbeddings], which every consumer
// reads as "no similarity search" rather than as a fault.
//
// It is also where the one rule the callers share lives: an error is no
// vector, never a failure to propagate. Every consumer of a vector here is
// ranking, and a ranking that could not be computed costs relevance rather
// than correctness.
//
// THE WHOLE TEXT, through [embeddings.EmbedWhole], because what these callers
// embed — a turn's ask, a completed turn — is text whose end matters as much
// as its beginning, and the provider refuses an input past the model's bound
// before sending it: handed Embed itself, every ask longer than one input
// would be no similarity search at all. A text within the bound is one piece
// and goes through Embed, so it is the vector it always was.
//
// BOUNDED BY [embeddings.EmbedTimeout] as a whole, the ceiling a single call
// has always had here: a long ask is one batch call, whose requests carry a
// corpus's ceiling rather than a turn start's, and a turn must not wait longer
// for its similarity search because the ask was long.
func (e *Engine) embedText(ctx context.Context, text string) ([]float32, error) {
	held := e.embeddings.Load()
	if held == nil || *held == nil {
		return nil, learning.ErrNoEmbeddings
	}
	batch, ok := (*held).(embeddings.BatchEmbedder)
	if !ok {
		return (*held).Embed(ctx, text)
	}
	ctx, cancel := context.WithTimeout(ctx, embeddings.EmbedTimeout)
	defer cancel()
	return embeddings.EmbedWhole(ctx, batch, text)
}
