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
		// THE CONFIGURATION'S RULE for whether a request asks for the
		// width, where each endpoint's documentation is cited.
		OmitDimensions: !cfg.SendsDimensions(),
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
//
// THE MODEL IS READ OFF THE SAME EMBEDDER the floats came from, in the same
// call, so a vector and the space it is tagged with can never describe two
// providers on either side of an apply.
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

// embedConfiguration is what a provider's refusals are a fact about: the model,
// the width, the limits it holds inputs and requests to, and the endpoint it
// sends to.
//
// ONE IDENTITY FOR BOTH MEMORIES this node keeps of what the provider refused —
// the knowledge corpus duty's ([embedDuty]) and the memory fill's
// ([memoryFill]) — so an apply cannot leave one loop holding what the provider
// refused while the other isolates it all again, or lift one loop's pause on
// a configuration concluded refused while the other's stands.
//
// # Why the configuration and not the provider
//
// Each memory used to belong to the provider's SLOT, which every apply fills
// with a provider built afresh — so an apply that changed nothing about the
// embeddings (a role added, a channel renamed) and re-activating an unchanged
// revision to rotate a key both started the memory again: every input the
// provider refuses was isolated once more, fifteen requests apiece, and a
// configuration concluded refused was sent to again at once, on every apply.
// A refusal is about what the provider will take; what moves that is exactly
// these four, so a change to any of them — a lowered `max_input_tokens`,
// another gateway, another model — starts a memory with nothing held, and
// nothing else does.
//
// THE KEY IS NOT IN IT, deliberately: rotating a credential is the documented
// gesture for a key that leaked, and it says nothing about which texts the
// model accepts. A key the provider does not take is not a refusal at all
// ([embeddings.ErrConfiguration]), and ends a pass without being remembered.
type embedConfiguration struct {
	model    string
	width    int
	limits   embeddings.Limits
	endpoint string
}

// configurationOf is the configuration provider embeds under.
//
// READ OFF THE PROVIDER THE CALLER EMBEDS WITH, never off the slot beside it,
// so no apply landing between two reads can pair one provider's refusals with
// another's.
//
// The endpoint is read only from a provider that reports one: the shipped
// provider does, and a provider that does not — a test's — is keyed on the
// other three.
func configurationOf(provider embeddings.Embedder) embedConfiguration {
	out := embedConfiguration{
		model: provider.Model(), width: provider.Width(), limits: provider.Limits(),
	}
	if at, ok := provider.(interface{ Endpoint() string }); ok {
		out.endpoint = at.Endpoint()
	}
	return out
}
