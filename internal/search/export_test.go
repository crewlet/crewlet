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
	set, err := readTrainingSet(ctx, tx, model, dim, unwatched)
	return TrainingReading{Corpus: set.corpus, Trials: set.trials,
		HeldOut: set.heldOut, IDs: set.ids}, err
}

// SearchWhile is [searchWhile] and P95ms [p95ms]: the searchers
// BenchmarkIVFTrainingShare runs beside a training, for the benchmarks that
// measure the rest of a training under the same load.
var (
	SearchWhile = searchWhile
	P95ms       = p95ms
)

// CodeFromBits is [codeFromBits], for the gate that holds it to [Quantize].
var CodeFromBits = codeFromBits

// ExactTops is [exactTops], the one ground truth, for the gate that holds it
// to the per-query statement it replaced — read the way the evaluation reads
// it, with no bound watching.
func ExactTops(ctx context.Context, tx *sql.Tx, queries []sampledDoc, shapes [][]ShapeQuery,
	model string, dim, limit int) ([][]ExactTop, error) {
	return exactTops(ctx, tx, queries, shapes, model, dim, limit, unwatched)
}

// ProgressStride is [progressStride].
const ProgressStride = progressStride

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
