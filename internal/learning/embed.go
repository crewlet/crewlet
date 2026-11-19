package learning

import (
	"context"
	"errors"
)

// How a learning path turns text into a vector.
//
// The embedder is a function read at call time, never captured at build: an
// apply builds tools before it stores the new embedder, so a captured one is an
// epoch stale (none on first boot, the retired key after a rotation). The
// function reads the CURRENT embedder per call, and "is there one" is its
// answer ([ErrNoEmbeddings]).
//
// A vector is a point in one model's space: two models of one width are two
// spaces, and a cosine across them looks like a ranking and ranks nothing. So
// the seam answers a [Vector], floats AND model read in the same call; every
// stored vector is tagged, and recall compares only rows of the query's model.

// Vector is one text's embedding and the model whose space it is in.
type Vector struct {
	// Values are the floats, as wide as the store's vector columns.
	Values []float32

	// Model is the id of the model that produced them.
	Model string
}

// Embed turns text into a vector, or reports why it cannot.
//
// [ErrNoEmbeddings] is the company having configured none, which every caller
// treats as a supported state rather than a failure; any other error is a
// provider that was asked and did not answer.
type Embed func(ctx context.Context, text string) (Vector, error)

// ErrNoEmbeddings reports that this company configures no embeddings
// provider, so nothing can be embedded at all.
//
// Its own sentinel, and errors.Is-comparable, because the two answers send a
// caller to opposite places: "no provider" is how this company is set up and
// is never logged as a fault, while a provider that failed is a fault and is
// the one an operator needs to see.
var ErrNoEmbeddings = errors.New("learning: no embeddings are configured")
