package executor

// NodeWork is what a node runs of this package's pools, which the engine says,
// as it alone reads the node's roles. The zero value runs none.
type NodeWork struct {
	// Boxes is whether the node hosts its seats' boxes: it collects their
	// runs, reads them for its peers, takes their sessions, reads what kept
	// sessions hold, and its seats read their transcripts.
	Boxes bool
	// Turns is how many turns the node runs at once (node.max_concurrent),
	// which bounds its reads of its runs' boxes too (internal/sandbox's
	// collections), and PeerReads how many reads of its boxes it makes at
	// once for its peers (the engine's boxReadsHeld): the base tier's count,
	// on a node hosting boxes.
	Turns, PeerReads int
	// PersonReads is whether the node answers people's reads of transcripts,
	// as its API does on a node serving ingress.
	PersonReads bool
}

// MemoryPools is the most w's pools hold at once on a node of memory bytes,
// each at its worst, grown by [heapGrowth]: what the node keeps beside Gitlet
// before its git lanes and background work are sized (internal/gitlet/gitproc's
// Options.Reserve; the package doc's "The memory a node keeps for these
// pools"). One term a pool; a pool whose worst is a share of the node's
// memory reads memory.
func (w NodeWork) MemoryPools(memory int64) int64 {
	live := int64(w.transcriptReads()) * TranscriptReadHeldBytes
	if w.Boxes {
		live += int64(w.Turns+w.PeerReads)*baseTierReadBytes +
			SessionTakesAtOnce*takeBaseHeldBytes +
			CollectionLargeReadsAtOnce*threadChainHeldBytes +
			HoldingsReadsAtOnce*threadChainHeldBytes
	}
	return heapGrowth * live
}

// transcriptReads is how many transcripts w's node decodes at once: every
// slot ([TranscriptReadsAtOnce]) where people read them, the seats' own
// ([SeatTranscriptReadsAtOnce]) where only its seats do, and none elsewhere.
func (w NodeWork) transcriptReads() int {
	switch {
	case w.PersonReads:
		return TranscriptReadsAtOnce
	case w.Boxes:
		return SeatTranscriptReadsAtOnce
	}
	return 0
}

// heapGrowth is how far the heap grows past what is live before the collector
// runs at Go's default GOGC, 100, which the engine runs at with no GOMEMLIMIT:
// to twice it. Every figure [MemoryPools] adds is live, and each peak measured
// at the default is within twice it (a transcript read's 58 MiB of 29, the
// chain's 70 of 43, a base pass's 8 of 5.3).
const heapGrowth = 2
