package learning

import (
	"context"
	"errors"
)

// How a learning path turns text into a vector.
//
// # A FUNCTION, READ AT CALL TIME
//
// The company's embedder is replaced on every config apply — a new model, a
// rotated key — and an apply builds its tools and workers BEFORE it stores the
// embedder it is applying. So a seam holding the embedder it was built with
// ran one epoch behind for its whole life: the first epoch a node booted had
// none at all, and re-activating an unchanged revision to rotate a credential
// (the documented gesture) left every holder calling the provider with the
// retired key. What each path is handed instead is a function that reads the
// engine's CURRENT embedder each time it is called, and "is there one" is that
// call's answer ([ErrNoEmbeddings]) rather than a nil checked once at build.

// Embed turns text into a vector, or reports why it cannot.
//
// [ErrNoEmbeddings] is the company having configured none, which every caller
// treats as a supported state rather than a failure; any other error is a
// provider that was asked and did not answer.
type Embed func(ctx context.Context, text string) ([]float32, error)

// ErrNoEmbeddings reports that this company configures no embeddings
// provider, so nothing can be embedded at all.
//
// Its own sentinel, and errors.Is-comparable, because the two answers send a
// caller to opposite places: "no provider" is how this company is set up and
// is never logged as a fault, while a provider that failed is a fault and is
// the one an operator needs to see.
var ErrNoEmbeddings = errors.New("learning: no embeddings are configured")
