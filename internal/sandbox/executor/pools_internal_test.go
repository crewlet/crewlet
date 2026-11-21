package executor

import "testing"

// A NODE KEEPS THE POOLS IT RUNS, EACH AT ITS WORST LIVE AND HALF AGAIN, the
// heap's room under the engine's memory limit: a node hosting boxes keeps a
// base read for each of its turns and its peers' reads, the takes at their
// base and both large slots, and its seats' one transcript read; one
// answering people keeps every transcript slot; one doing neither keeps
// nothing.
//
// Mutations, each red: keep no room, or the default GOGC's twice live; count
// the turns or the peers' reads off a node hosting boxes, or neither on one;
// count people's reads where only seats read, or the seats' where people do.
func TestANodeKeepsThePoolsItRunsWithTheLimitsRoom(t *testing.T) {
	t.Parallel()
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
		if got, want := tc.work.MemoryPools(16<<30), tc.live+tc.live/2; got != want {
			t.Errorf("%s (%+v): the node keeps %d bytes for the executor's pools, want %d", tc.name, tc.work,
				got, want)
		}
	}
}
