package search

import (
	"context"
	"database/sql"
	"time"
)

// SemanticStatement is [semanticStatement], for the plan gate: it explains the
// statement [Semantic] issues rather than a copy that can drift from it.
var SemanticStatement = semanticStatement

// The lexical index's statements, for its plan gate, on [SemanticStatement]'s
// terms.
const (
	LexicalCorpusStatement        = lexicalCorpusStatement
	LexicalTermDocsStatement      = lexicalTermDocsStatement
	LexicalClearPostingsStatement = lexicalClearPostingsStatement
	LexicalRemoveDocStatement     = lexicalRemoveDocStatement
	LexicalOrphanWalkStatement    = lexicalOrphanWalkStatement
	LexicalIndexedCountStatement  = lexicalIndexedCountStatement
	LinkedFromStatement           = linkedFromStatement
)

// The lexical index's statement builders, on the same terms.
var (
	LexicalPostingScanStatement = lexicalPostingScanStatement
	LexicalHydrateStatement     = lexicalHydrateStatement
	LexicalHeldStatement        = lexicalHeldStatement
)

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

// HeldOf is a [Held] passing over docs, each at the version and title it is
// given, as the refusal memory builds one for a corpus ([Refusals.plan]) — for
// the gates that hold a corpus's own selection to the pass-over, which no
// other caller of [Corpus.Stale] reaches.
func HeldOf(docs ...Document) Held {
	held := Held{byID: map[string]heldAs{}}
	for _, doc := range docs {
		held.byID[doc.ID] = heldAs{version: doc.Version, title: doc.Title}
	}
	return held
}

// EmbedReadChars is [embedReadChars], for the gate that holds a selection to
// reading only the opening of a body.
const EmbedReadChars = embedReadChars

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

// RefileStatement is [refileStatement], the re-filing a reassign batch runs.
const RefileStatement = refileStatement

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
	StoredVectorStatement  = storedVectorStatement
)

// SetClock times f's passes by now, for the cases that let a pass lapse.
func SetClock(f *FanOut, now func() time.Time) { f.now = now }

// PassOverFor is [passOverFor].
const PassOverFor = passOverFor

// The chat index's statements, for its plan gate: it explains what the index
// issues rather than copies that can drift from it.
var (
	ChatChangedReachStatement     = chatChangedReachStatement
	ChatChangedRowsStatement      = chatChangedRowsStatement
	ChatErasedReachStatement      = chatErasedReachStatement
	ChatErasedRowsStatement       = chatErasedRowsStatement
	ChatPastWatermarkStatement    = chatPastWatermarkStatement
	ChatPastErasedStatement       = chatPastErasedStatement
	ChatLiveOldestStatement       = chatLiveOldestStatement
	ChatHeldOldestStatement       = chatHeldOldestStatement
	ChatRoomRotationStatement     = chatRoomRotationStatement
	ChatHeldDocStatement          = chatHeldDocStatement
	ChatDocTermsStatement         = chatDocTermsStatement
	ChatClearPostingsStatement    = chatClearPostingsStatement
	ChatRemoveDocStatement        = chatRemoveDocStatement
	ChatDocUpsertStatement        = chatDocUpsertStatement
	ChatRangeSizeStatement        = chatRangeSizeStatement
	ChatRangeTermsStatement       = chatRangeTermsStatement
	ChatRangeDropStatement        = chatRangeDropStatement
	ChatWatermarkStatement        = chatWatermarkStatement
	ChatErasedThroughStatement    = chatErasedThroughStatement
	ChatAdvanceWatermarkStatement = chatAdvanceWatermarkStatement
	ChatAdvanceErasedStatement    = chatAdvanceErasedStatement
	ChatBeginRebuildStatement     = chatBeginRebuildStatement
	ChatStateStatement            = chatStateStatement
	ChatRecordInstanceStatement   = chatRecordInstanceStatement
	ChatDropBatchStatement        = chatDropBatchStatement
	ChatRebuiltStatement          = chatRebuiltStatement
	ChatCorpusStatement           = chatCorpusStatement
	ChatRebuiltDrops              = chatRebuiltDrops
	ChatRoomsCorpusStatement      = chatRoomsCorpusStatement
	ChatTermDocsStatement         = chatTermDocsStatement
	ChatPostingScanStatement      = chatPostingScanStatement
	ChatHydrateStatement          = chatHydrateStatement
	ChatEmptiedRoomsStatement     = chatEmptiedRoomsStatement
	ChatEmptiedTermsStatement     = chatEmptiedTermsStatement
)

// MaxPostingScan is [maxPostingScan], for the gate that holds which postings a
// frequent chat term keeps.
const MaxPostingScan = maxPostingScan
