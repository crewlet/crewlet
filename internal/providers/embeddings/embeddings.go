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
