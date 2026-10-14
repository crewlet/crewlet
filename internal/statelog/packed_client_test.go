package statelog_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/statelog"
)

// HELD HERE, BESIDE THE NUMBER: the stride and the packing are this package's
// ([statelog.GenerationStride], [statelog.Position.Packed]), and a gate on a
// client's copy of a value belongs to the package that owns the value — as
// MAX_IN_FLIGHT_QUERIES's does in internal/api/stream. The chat screen is
// simply the client that packs one.

// TestTheDashboardPacksAReadCursorAsTheEngineDoes holds the stride the chat
// screen packs a read cursor with against [statelog.GenerationStride].
//
// A READ CURSOR IS A PACKED POSITION. The coordination record holds one
// integer per room, and the unread count compares it with the packed position
// of the room's newest message ([statelog.Position.Packed]) — so the screen,
// which packs the position of the last message it showed before it flushes,
// has to pack in the engine's number space. It is not a bound and cannot be
// refused on, and a drift is silent in both directions: a stride too high
// writes a cursor above every message there will ever be, and the room reads
// as read for ever; too low, and the badge never clears however far somebody
// reads.
//
// THE NUMBER, AND WHAT IT DOES TO A POSITION: the client's packing is
// `generation * GENERATION_STRIDE + seq` (`routes/chat/cursor.ts`), so the
// probes are that formula against the engine's own [statelog.Position.Packed]
// at the edges a stride decides — a generation's first sequence, its last, and
// the next generation's first.
func TestTheDashboardPacksAReadCursorAsTheEngineDoes(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "GENERATION_STRIDE")
	if err != nil {
		t.Fatal(err)
	}
	stride, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		t.Fatalf("GENERATION_STRIDE is %q, which is not an integer: %v", raw, err)
	}
	if stride != statelog.GenerationStride {
		t.Errorf("the dashboard packs a read cursor with a stride of %d and the "+
			"engine packs positions with %d, so every cursor the screen writes is "+
			"in another number space from the messages it is compared with. "+
			"statelog owns the value, so contract/chat.ts is what changes",
			stride, int64(statelog.GenerationStride))
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
