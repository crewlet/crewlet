package executor

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A NODE KEEPS THE POOLS IT RUNS, EACH AT ITS WORST LIVE AND HALF AGAIN, the
// heap's room under the engine's memory limit: a node hosting boxes keeps a
// base read for each of its turns and its peers' reads, the takes at their
// base, the large slot at its holder's worst and a reading's slot at a chain,
// and its seats' one transcript read; one answering people keeps every
// transcript slot; one doing neither keeps nothing.
//
// Mutations, each red: keep no room, or the default GOGC's twice live; count
// the turns or the peers' reads off a node hosting boxes, or neither on one;
// count people's reads where only seats read, or the seats' where people do;
// count the large slot at a chain alone.
func TestANodeKeepsThePoolsItRunsWithTheLimitsRoom(t *testing.T) {
	t.Parallel()
	boxes := NodeWork{Boxes: true, Turns: 32, PeerReads: 64}
	seatsLive := int64(96*baseTierReadBytes) + SessionTakesAtOnce*takeBaseHeldBytes +
		CollectionLargeReadsAtOnce*largeReadHeldBytes() + HoldingsReadsAtOnce*threadChainHeldBytes
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

// THE LARGE SLOT IS COUNTED AT THE LARGER OF ITS TWO HOLDERS, never their sum:
// one pass holds it at a time, a collection's with its chain or a take's, its
// scrub at the plaintext's bound and the longest value it can hold beside the
// most its meter holds, a Claude Code meter's line or a Gemini CLI count at its
// ids' bound, all past the base the take's own slot already counts, each
// figure from its own definition. A take's outweighs a chain's, and a Gemini
// CLI count a Claude Code meter's line.
//
// Mutations, each red: count the slot at a chain alone, or at the chain and
// the take summed; leave the meter out of the take, count a Claude Code
// meter's line alone, or count a meter's base again; count the scrub's base
// again; size the scrub with no value, or at a reading of nothing.
func TestTheLargeSlotIsCountedAtTheLargerOfItsTwoHolders(t *testing.T) {
	t.Parallel()
	chain := int64(threadChainHeldBytes + summaryLargeHeldBytes)
	line := int64(threadLineMax-collectionBaseLineCharge) * 5 / 4
	count := int64(geminiCountLargeHeldBytes - meterBaseHeldBytes)
	take := scrubHeldBytes(scrubPlainMax, scrubLongestMax) - scrubBaseHeldBytes + max(line, count)
	if take == chain || count <= line {
		t.Fatalf("the premise: a take past its base holds %d bytes, what a chain's pass does beside its summary, "+
			"or a Gemini CLI count past its base %d, no more than a Claude Code meter's line %d", take, count, line)
	}
	if got := largeReadHeldBytes(); got != max(chain, take) {
		t.Errorf("the large slot is counted at %d bytes, want the larger of a chain's beside its summary's %d "+
			"and a take's %d", got, chain, take)
	}
}

// THE SCRUB'S LONGEST VALUE IS WITHIN ITS BOUND: a capture's values at their
// bound in one line of control bytes, which its JSON form writes in six bytes
// each, make the longest value the scrub holds, exactly the figure its memory
// is counted at; invalid bytes, three bytes each in that form, make less.
//
// Mutation, red: count the longest value at fewer than six bytes a byte.
func TestTheScrubsLongestValueIsWithinItsBound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		b     string
		exact bool
	}{{"\x01", true}, {"\xff", false}} {
		worst := strings.Repeat(tc.b, sandbox.MaxScrubBytes)
		got := newScrubber([]string{worst}).values.longest
		if got > scrubLongestMax || tc.exact && got != scrubLongestMax {
			t.Errorf("a capture's values at their bound, every byte %q, make a longest value of %d bytes, "+
				"want scrubLongestMax, %d, exact %t", tc.b, got, scrubLongestMax, tc.exact)
		}
	}
}
