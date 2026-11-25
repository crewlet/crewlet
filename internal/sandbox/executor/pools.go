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
// each at its worst live with the heap's room under the engine's memory limit
// ([limitHeadroomHalves]; internal/engine's limitMemory): what the node keeps
// beside Gitlet before its git lanes and background work are sized
// (internal/gitlet/gitproc's Options.Reserve; the package doc's "The memory a
// node keeps for these pools"). One term a pool; a pool whose worst is a share
// of the node's memory reads memory.
func (w NodeWork) MemoryPools(memory int64) int64 {
	live := int64(w.transcriptReads()) * TranscriptReadHeldBytes
	if w.Boxes {
		live += int64(w.Turns+w.PeerReads)*baseTierReadBytes +
			SessionTakesAtOnce*takeBaseHeldBytes +
			CollectionLargeReadsAtOnce*largeReadHeldBytes() +
			HoldingsReadsAtOnce*threadChainHeldBytes
	}
	return live * limitHeadroomHalves / 2
}

// largeReadHeldBytes is what the large slot ([CollectionLargeReadsAtOnce])
// adds at its worst: the larger of its holders' figures, never their sum. Each
// pass takes it through its own [passBudget] (readMainThread's, take.go's), so
// one holds it at a time; only a collection's builds a chain, and a take's is
// its scrub and its meter at once, counted past its base ([takeLargeHeldBytes]).
// The chain's is whole, its base's 2.7 MiB in it, which the max never reads; a
// collection's Gemini CLI count past its base holds less than a chain
// ([geminiCountLargeHeldBytes]).
func largeReadHeldBytes() int64 {
	return max(threadChainHeldBytes, takeLargeHeldBytes())
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

// limitHeadroomHalves is the heap's room under the engine's memory limit, in
// halves of what is live: 1.5x. With none the collector never pauses (the
// scrub 2.3x slower); with half again every wait holds. Measured, GOMEMLIMIT at
// the binary's runtime overhead plus live x h, on two cores, beside the default:
//
//	h                     1.25         1.5         2            default
//	main thread, empty    25.5-26.2 s  23.5 s      22.5-23.3 s  22.3 s
//	main thread, ordinary 3.7-4.2 s    2.8-2.9 s   2.35-2.4 s   2.34 s
//	scrub, a pwd string   146 s        138-139 s   136 s        136 s
//
// The scrub's slowest shape at 1.5, a string torn at its last line (183 s),
// leaves internal/sandbox's SessionScrubWait 21 s.
const limitHeadroomHalves = 3
