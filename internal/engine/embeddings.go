package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// The vector backend, wired. It is optional: with no providers.embeddings the
// diary's pool falls back to recency and episode recall renders nothing (it
// cannot claim similarity with none behind it), both first-class states
// rather than failures. So nil is returned freely, but no
// default is invented: the store's vector columns are sized from the
// configured width at open, and an unrequested embedder would write rows at
// whatever width its model produces.

// buildEmbedder constructs the company's embedder, or nil.
//
// PER EPOCH, since an apply can change the model, but NOT the width: the store
// was sized at open, so a revision that moves it is refused here, where an
// operator is watching, rather than writing rows the reader cannot match.
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
	// The provider refuses an input past these before sending it and packs
	// batches to fit them.
	limits := cfg.Limits()
	provider, err := embeddings.New(embeddings.Config{
		Model:      env.Value(cfg.Model),
		Dimensions: cfg.Width(),
		// The configuration's rule, where each endpoint's docs are cited.
		OmitDimensions: !cfg.SendsDimensions(),
		// The configuration's rule, OPENAI_API_KEY included, resolved through
		// the store-aware chain.
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

// embedText is the company's embedder as the learning paths take it (the
// [learning.Embed] seam): READ AT CALL TIME, never captured, because an apply
// equips its tools BEFORE it stores its embedder ([Engine.equip]), so a
// captured seam runs an epoch behind. None configured answers
// [learning.ErrNoEmbeddings], "no similarity search". An error is no vector,
// never a failure to propagate: each caller only ranks.
//
// THE WHOLE TEXT, through [embeddings.EmbedWhole]: an ask's end matters, and
// the provider refuses an input past the model's bound. The whole call is
// bounded by [embeddings.EmbedTimeout], so a long ask does not delay a turn.
// The model is read off the same embedder as the floats, so a vector's tag
// cannot name another provider across an apply.
func (e *Engine) embedText(ctx context.Context, text string) (learning.Vector, error) {
	held := e.embeddings.Load()
	if held == nil || *held == nil {
		return learning.Vector{}, learning.ErrNoEmbeddings
	}
	embedder := *held
	var (
		values []float32
		err    error
	)
	if batch, ok := embedder.(embeddings.BatchEmbedder); ok {
		bounded, cancel := context.WithTimeout(ctx, embeddings.EmbedTimeout)
		defer cancel()
		values, err = embeddings.EmbedWhole(bounded, batch, text)
	} else {
		values, err = embedder.Embed(ctx, text)
	}
	if err != nil {
		return learning.Vector{}, err
	}
	return learning.Vector{Values: values, Model: embedder.Model()}, nil
}

// embedConfiguration is what a provider's refusals are a fact about: the
// model, width, limits and endpoint. Keyed on these rather than on the
// provider, which every apply rebuilds, so only a change to one of them starts
// the refusal memory afresh.
//
// ONE IDENTITY FOR BOTH MEMORIES ([embedDuty]'s and [memoryFill]'s), so an
// apply cannot reset one while the other's stands.
//
// THE KEY IS NOT IN IT: rotating a leaked key says nothing about which texts
// the model accepts. A key the provider rejects is no refusal at all
// ([embeddings.ErrConfiguration]) and is not remembered.
type embedConfiguration struct {
	model    string
	width    int
	limits   embeddings.Limits
	endpoint string
}

// configurationOf is the configuration provider embeds under, READ OFF THE
// PROVIDER THE CALLER EMBEDS WITH, never the slot beside it, so an apply
// between two reads cannot pair one provider's refusals with another's. A
// provider reporting no endpoint (a test's) is keyed on the other three.
func configurationOf(provider embeddings.Embedder) embedConfiguration {
	out := embedConfiguration{
		model: provider.Model(), width: provider.Width(), limits: provider.Limits(),
	}
	if at, ok := provider.(interface{ Endpoint() string }); ok {
		out.endpoint = at.Endpoint()
	}
	return out
}
