package executor

import "github.com/crewlet/crewlet/internal/queue"

// MemoryPools is the most this process's executor pools hold at once, each at
// its worst, on a node of memory bytes: what a node running seats beside
// Gitlet keeps out of the memory its git lanes and background work are sized
// by (internal/gitlet/gitproc's Options.Reserve; the package doc's "The
// memory a node keeps for these pools"). One term a pool; a pool whose worst
// is a share of the node's memory reads memory.
func MemoryPools(memory int64) int64 {
	return baseTierBytes +
		takesBaseBytes +
		largeTierBytes
}

// The pools [MemoryPools] adds up, each from where it is measured: the base
// tier, the box host's reads for its peers (queue.ConcurrentAnswers, the
// engine's boxReadsHeld) at [baseTierReadBytes]; the takes at their base,
// [SessionTakesAtOnce] at takeBaseBytes, the scrub's 8.22 MiB and the meter's
// 3.34 (take.go); and the large tier: [TranscriptReadsAtOnce] reads at
// transcriptReadHeldBytes (a line's decode, 134 MiB, and its redaction, 93),
// and a collection's large pass and a reading at threadModelBytes each, the
// chain at both bounds (the package doc's "A collection's read of a thread").
const (
	baseTierBytes           = queue.ConcurrentAnswers * baseTierReadBytes
	takeBaseBytes           = 11838 << 10
	takesBaseBytes          = SessionTakesAtOnce * takeBaseBytes
	transcriptReadHeldBytes = 227 << 20
	threadModelBytes        = 43 << 20
	largeTierBytes          = TranscriptReadsAtOnce*transcriptReadHeldBytes +
		(CollectionLargeReadsAtOnce+HoldingsReadsAtOnce)*threadModelBytes
)

// The takes at their base hold no more than the base tier, which
// [SessionTakesAtOnce] is sized by: a negative constant does not compile.
const _ = uint(baseTierBytes - takesBaseBytes)
