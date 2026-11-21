package executor

import "testing"

// A NODE KEEPS THE POOLS IT RUNS, EACH AT ITS WORST AND GROWN BY THE HEAP: a
// node hosting boxes keeps a base read for each of its turns and its peers'
// reads, the takes at their base and both large slots, and its seats' one
// transcript read; one answering people keeps every transcript slot; one
// doing neither keeps nothing. Each pool, grown, holds the peak its benchmark
// measured at the default GOGC.
//
// Mutations, each red: count no heap growth; count the turns or the peers'
// reads off a node hosting boxes, or neither on one; count people's reads
// where only seats read, or the seats' where people do.
func TestANodeKeepsThePoolsItRunsGrownByTheHeap(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	for _, peak := range []struct {
		pool       string
		live, peak int64
	}{
		{"a transcript read (BenchmarkAPersonsReadAtItsBounds)", TranscriptReadHeldBytes, 58 * mib},
		{"the chain at both bounds (BenchmarkAReadingAtItsBounds)", threadChainHeldBytes, 70 * mib},
		{"a base pass (BenchmarkACollectionPassAtItsBase)", baseTierReadBytes, 8 * mib},
	} {
		if heapGrowth*peak.live < peak.peak {
			t.Errorf("%s keeps %d bytes grown, short of its %d-byte peak at the default GOGC", peak.pool,
				heapGrowth*peak.live, peak.peak)
		}
	}
	boxes := NodeWork{Boxes: true, Turns: 32, PeerReads: 64}
	seatsLive := int64(96*baseTierReadBytes) + SessionTakesAtOnce*takeBaseHeldBytes +
		(CollectionLargeReadsAtOnce+HoldingsReadsAtOnce)*threadChainHeldBytes
	for _, tc := range []struct {
		name string
		work NodeWork
		live int64
	}{
		{"none", NodeWork{}, 0},
		{"turns and peers' reads alone, hosting no box", NodeWork{Turns: 32, PeerReads: 64}, 0},
		{"people's reads", NodeWork{PersonReads: true}, TranscriptReadsAtOnce * TranscriptReadHeldBytes},
		{"boxes", boxes, seatsLive + SeatTranscriptReadsAtOnce*TranscriptReadHeldBytes},
		{"boxes and people's reads", NodeWork{Boxes: true, Turns: 32, PeerReads: 64, PersonReads: true},
			seatsLive + TranscriptReadsAtOnce*TranscriptReadHeldBytes},
		{"boxes, one turn more", NodeWork{Boxes: true, Turns: 33, PeerReads: 64},
			seatsLive + baseTierReadBytes + SeatTranscriptReadsAtOnce*TranscriptReadHeldBytes},
	} {
		if got, want := tc.work.MemoryPools(16<<30), heapGrowth*tc.live; got != want {
			t.Errorf("%s (%+v): the node keeps %d bytes for the executor's pools, want %d", tc.name, tc.work,
				got, want)
		}
	}
}
