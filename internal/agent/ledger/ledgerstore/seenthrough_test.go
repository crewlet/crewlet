package ledgerstore_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A MARK MOVES FORWARD ONLY: two nodes writing 40 and 35 leave 40, a later
// position or generation moves it, and a position on another log is a mark of
// its own that leaves this one where it was.
func TestASeenThroughMarkMovesForwardOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seen := ledgerstore.NewMemorySeenThrough()
	at := func(gen uint32, seq uint64) statelog.Position {
		return statelog.Position{Stream: "worklet", Generation: gen, Seq: seq}
	}
	for _, step := range []struct {
		advance statelog.Position
		want    statelog.Position
	}{
		{at(1, 40), at(1, 40)},
		{at(1, 35), at(1, 40)},
		{at(1, 44), at(1, 44)},
		{at(2, 3), at(2, 3)},
		{at(1, 90), at(2, 3)},
		{statelog.Position{Stream: "chatlet", Generation: 1, Seq: 7}, at(2, 3)},
	} {
		if err := seen.Advance(ctx, "pm", "worklet:W-1", step.advance); err != nil {
			t.Fatalf("advance to %v: %v", step.advance, err)
		}
		if got := seen.Mark(ctx, "pm", "worklet:W-1", "worklet"); got != step.want {
			t.Errorf("after %v the mark is %v, want %v", step.advance, got, step.want)
		}
	}
	if got := seen.Mark(ctx, "pm", "worklet:W-1", "chatlet"); got.Seq != 7 {
		t.Errorf("the other log's mark is %v, want its own position 7", got)
	}
	if err := seen.Advance(ctx, "pm", "worklet:W-1", statelog.Position{}); err != nil {
		t.Errorf("a position naming no log: %v, want it ignored", err)
	}
}
