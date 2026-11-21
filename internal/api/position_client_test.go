package api_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DASHBOARD UNPACKS AND PACKS A POSITION WITH THE ENGINE'S OWN STRIDE,
// held here (see internal/clientsource). Coverage answers carry
// `applied_through` and `log_seq` packed, split by the stride in
// dashboard/src/contract/positions.ts; a drifted one still compares correctly
// and prints every re-anchored position wrong, unnoticed. The chat screen
// packs its read cursor with the same stride
// (dashboard/src/routes/chatlet/cursor.ts) and the unread count compares it
// with [statelog.Position.Packed]: too high, the room reads as read for ever;
// too low, the badge never clears. The probes are
// `generation * GENERATION_STRIDE + seq` against the engine's packing at a
// generation's first and last sequence and the next one's first.
func TestTheDashboardUnpacksAPositionWithTheEnginesStride(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "GENERATION_STRIDE")
	if err != nil {
		t.Fatal(err)
	}
	// Base 0 reads the literal as TypeScript spells it, digit separators and
	// all.
	stride, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		t.Fatalf("GENERATION_STRIDE = %q is not a number: %v", raw, err)
	}
	if stride != statelog.GenerationStride {
		t.Errorf("the dashboard unpacks a position with a stride of %d, where the "+
			"engine packs one with %d: a re-anchored log's position would print as "+
			"the wrong generation and sequence, and every read cursor the chat "+
			"screen writes is in another number space from the messages it is "+
			"compared with", stride, int64(statelog.GenerationStride))
	}
	// ANY LOG: a stride is the same arithmetic on every one of them.
	const stream = "CREWLET_CHAT_LOG"
	for _, at := range []statelog.Position{
		{Stream: stream, Generation: 0, Seq: 7},
		{Stream: stream, Generation: 0, Seq: statelog.GenerationStride - 1},
		{Stream: stream, Generation: 1, Seq: 0},
		{Stream: stream, Generation: 3, Seq: 41},
	} {
		if client := int64(at.Generation)*stride + int64(at.Seq); client != at.Packed() {
			t.Errorf("%v packs to %d on the screen and %d in the engine", at, client, at.Packed())
		}
	}
}
