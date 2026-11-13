package turnctx_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
)

// A TURN KEEPS ITS NEWEST ANSWERS WITHIN ITS BOUND: past the bound the oldest
// answers go first, each counted against its tool so a citation of a dropped
// call is told apart from one of a call never made; an answer larger than the
// whole bound is not kept; and a nil record has seen nothing.
//
// Mutation: drop the newest answer instead of the oldest, and the call a turn
// just made cannot be cited.
func TestATurnKeepsItsNewestAnswersWithinItsBound(t *testing.T) {
	t.Parallel()
	seen := turnctx.NewSeen()
	chunk := strings.Repeat("x", turnctx.MaxSeenBytes/4)
	for _, tool := range []string{"first", "second", "third", "fourth", "fifth"} {
		seen.Note(turnctx.Answer{Tool: tool, Output: chunk})
	}
	if kept, dropped := seen.Of("first"); len(kept) != 0 || dropped != 1 {
		t.Errorf("the oldest answer: %d kept, %d dropped; want it dropped and counted", len(kept), dropped)
	}
	if kept, dropped := seen.Of("fifth"); len(kept) != 1 || dropped != 0 {
		t.Errorf("the newest answer: %d kept, %d dropped; want it kept", len(kept), dropped)
	}
	seen.Note(turnctx.Answer{Tool: "huge", Output: strings.Repeat("x", turnctx.MaxSeenBytes+1)})
	if kept, dropped := seen.Of("huge"); len(kept) != 0 || dropped != 1 {
		t.Errorf("an answer past the whole bound: %d kept, %d dropped; want it counted and not kept", len(kept), dropped)
	}
	if kept, _ := seen.Of("fifth"); len(kept) != 1 {
		t.Error("an answer too large to keep pushed out one the turn held")
	}

	var none *turnctx.Seen
	none.Note(turnctx.Answer{Tool: "first", Output: "kept nowhere"})
	if kept, dropped := none.Of("first"); len(kept) != 0 || dropped != 0 {
		t.Errorf("a nil record holds %d answers (%d dropped)", len(kept), dropped)
	}
	var turn *turnctx.Turn
	if turn.Answers() != nil {
		t.Error("no turn has seen something")
	}
}
