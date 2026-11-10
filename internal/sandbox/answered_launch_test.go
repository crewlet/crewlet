package sandbox

import (
	"slices"
	"testing"
)

// THE ANSWERS THAT WOKE A TURN RIDE ITS RUN'S ROW, so the resume that carries
// the turn on, on any node, reads them back.
//
// Mutation: leave Answered off the row the launch writes, and this goes red.
func TestTheAnswersThatWokeATurnRideItsRow(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.Turn.Answered = []string{"a-1", "a-2"}
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := rig.get("t1").Answered; !slices.Equal(got, []string{"a-1", "a-2"}) {
		t.Errorf("the row carries answers %v, want the turn's", got)
	}
}
