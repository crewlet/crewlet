// Package embeddings is the vector backend behind diary and episode recall.
package embeddings

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// # What a vector is FOR here, and what follows from that
//
// Two things ask for one: a diary candidate search, and episode recall. Both
// are RANKING questions — "which of my memories resemble this task" — and
// neither is authoritative: the memory search hands its candidates to a model
// that decides, and recall renders a handful of past turns as background.
//
// So a failure costs relevance, never correctness. Every consumer treats an
// unavailable embedder as "no similarity search this turn" and carries on
// with what it has, which is why nothing here retries, queues or falls back
// to a second provider: the caller's degradation is cheaper than any of them.
//
// # The width is a contract with the STORE, not with the model
//
// The vector columns are sized once, at open, from providers.embeddings
// .dimensions. A model that produces a different width does not degrade a
// search — it writes rows that cannot be read back. So the width is checked
// against what the provider actually returns, on the first call, and a
// mismatch is refused loudly rather than stored.

// Embedder turns text into a vector.
//
// One method, and text-at-a-time rather than a batch: the two callers embed
// exactly one thing — the turn's task, or an episode's summary — and a batch
// API would have every caller build a slice of one.
type Embedder interface {
	// Embed returns the vector for text. An error means no vector, which
	// every caller reads as "no similarity search", never as a failure to
	// propagate.
	Embed(ctx context.Context, text string) ([]float32, error)

	// Width is the vector width this embedder produces, as configured.
	Width() int
}

// BatchEmbedder embeds many texts in ONE call, in the order they were given.
//
// # Why this is a second interface rather than a wider Embedder
//
// The two original callers embed exactly one thing — the turn's task, or an
// episode's summary — and a batch API would have both build a slice of one.
// The knowledge corpus is the caller that changed the arithmetic: filling a
// company's 110 000 sources one at a time is 110 000 round trips, which at a
// tenth of a second apiece does not fit in the tick it runs on, let alone in
// the eight hours a cold fill is budgeted at. Batched it is ≈ 860 requests
// covering the same inputs, billed identically because the provider bills per
// input TOKEN.
//
// Kept apart from [Embedder] so a backend that cannot batch is still a
// complete embedder: the caller asks for this interface and falls back to the
// one-at-a-time path, rather than every provider growing a method most of them
// would implement as a loop.
type BatchEmbedder interface {
	Embedder

	// EmbedBatch returns one vector per input, IN ORDER and one-to-one:
	// a caller matches results to inputs positionally, so a provider that
	// dropped an empty input would silently re-file every vector after it
	// onto the wrong document.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// ErrEmpty reports text with nothing in it.
//
// Its own error because the caller's answer differs: an empty task is not a
// provider problem and must not be logged as one, but it is still no vector.
var ErrEmpty = errors.New("embeddings: nothing to embed")

// checkedWidth verifies a returned vector against the configured width.
//
// ON EVERY CALL rather than only the first. A provider behind an aggregator
// can change model mid-deployment, and the failure that catches — rows
// written at the wrong width — is silent and permanent: the store accepts
// the write and the read never matches. One length comparison per call is
// nothing beside the round trip that produced it.
func checkedWidth(vector []float32, want int, model string) ([]float32, error) {
	if len(vector) != want {
		return nil, fmt.Errorf("embeddings: %s returned a %d-wide vector but "+
			"providers.embeddings.dimensions says %d — the store's columns are "+
			"sized from the config, so these rows could be written and never "+
			"read back", model, len(vector), want)
	}
	return vector, nil
}

// normalize prepares text for embedding.
//
// Collapsing whitespace is not cosmetic: the two callers pass a rendered
// task and a rendered summary, both of which carry the newlines and
// indentation of whatever produced them, and an embedding of the same
// sentence formatted two ways is two different vectors.
func normalize(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
