package search

import (
	"context"
	"database/sql"
)

// SemanticStatement is [semanticStatement], for the plan gate: it explains the
// statement [Semantic] issues rather than a copy that can drift from it.
var SemanticStatement = semanticStatement

// TrainingReading is what [readTrainingSet] reads, exported so a test trains
// an index from a store's rows exactly as the duty does.
type TrainingReading struct {
	Corpus  TrainingCorpus
	Trials  []IVFTrial
	HeldOut []int
	IDs     map[Source][]string
}

// ReadTrainingSet is [readTrainingSet].
func ReadTrainingSet(ctx context.Context, tx *sql.Tx, model string, dim int) (TrainingReading, error) {
	set, err := readTrainingSet(ctx, tx, model, dim)
	return TrainingReading{Corpus: set.corpus, Trials: set.trials,
		HeldOut: set.heldOut, IDs: set.ids}, err
}

// CodeFromBits is [codeFromBits], for the gate that holds it to [Quantize].
var CodeFromBits = codeFromBits

// ExactTops is [exactTops], the one ground truth, for the gate that holds it
// to the per-query statement it replaced.
var ExactTops = exactTops

// SampleDocuments is [sampleDocuments], and ShapesFor [shapesFor].
var (
	SampleDocuments = sampleDocuments
	ShapesFor       = shapesFor
)

// SampledDoc is [sampledDoc].
type SampledDoc = sampledDoc

// IndexStateOf reads what a duty tick would decide from.
func IndexStateOf(ctx context.Context, e *Embedder, dim int) (IndexState, error) {
	return e.indexState(ctx, dim)
}

// ReassignStatement is [reassignStatement], over a range.
var ReassignStatement = reassignStatement

// The statements the plan gate explains, as the code runs them.
var (
	ExactTopsStatement     = exactTopsStatement
	MatchingCountStatement = matchingCountStatement
)

const (
	IndexHeadStatement     = indexHeadStatement
	SpaceCountStatement    = spaceCountStatement
	TrainingCodesStatement = trainingCodesStatement
	StaleKeysStatement     = staleKeysStatement
	StaleRowsStatement     = staleRowsStatement
)
