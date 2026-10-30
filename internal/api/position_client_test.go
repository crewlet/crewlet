package api_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DASHBOARD UNPACKS AND PACKS A POSITION WITH THE ENGINE'S OWN STRIDE,
// held here — see internal/clientsource for why a copy exists at all and why
// this side checks it.
//
// Every coverage answer carries `applied_through` and `log_seq` packed, and the
// dashboard splits them into a generation and a sequence to print them
// (dashboard/src/components/work.tsx), with the stride its contract declares
// (dashboard/src/contract/positions.ts). A stride that drifted from the engine's
// would still compare correctly — the packed values are compared whole — and
// would print every re-anchored position as a wrong generation and a wrong
// sequence, which nothing else would notice.
//
// AND THE CHAT SCREEN PACKS A READ CURSOR WITH THE SAME ONE: the coordination
// record holds one integer per room, and the unread count compares it with the
// packed position of the room's newest message ([statelog.Position.Packed]),
// so the screen — which packs the position of the last message it showed
// before it flushes (dashboard/src/routes/chatlet/cursor.ts) — has to pack in the
// engine's number space. Drift there is silent in both directions: a stride
// too high writes a cursor above every message there will ever be, and the
// room reads as read for ever; too low, and the badge never clears. So the
// probes are the client's formula, `generation * GENERATION_STRIDE + seq`,
// against the engine's own packing at the edges a stride decides — a
// generation's first sequence, its last, and the next generation's first.
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
