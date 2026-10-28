package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// AN ANSWER OR COMPLETION FOR A SEAT THIS NODE HAS LET GO OF IS HANDED ON ONCE.
// A Nak spent one of the message's deliveries on every redelivery to a node
// that kept refusing it, and an answer walked to the dead-letter subject that
// way. The node defers it instead: one delivery, back to the head of the topic
// for the seat's holder, and the attachment quiesced so it does not come back
// here. Only when the seat host agrees it does not hold the seat: the run
// store's word alone, on a seat the host holds, is a disagreement the heartbeat
// settles, and quiescing a held seat's control topic would leave it deaf.
//
// Mutation: defer on the sentinel without asking the host, and the held seat's
// case quiesces; drop the rule, and the lost seat's case is a Nak.
func TestAControlDeliveryForALostSeatIsHandedOnOnce(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, nil)
	applyOK(t, e, sandboxDoc(""))
	waitHeld(t, e, "swe")
	refused := func(handle string) error {
		return fmt.Errorf("sandbox: %s is still owed this answer: sandbox: recording the "+
			"answer run t1 was given: %w", handle, sandbox.ErrSeatNotHeld)
	}
	for name, tc := range map[string]struct {
		handle string
		err    error
		want   queue.Outcome
	}{
		"a seat this node no longer holds": {handle: "nobody", err: refused("nobody"), want: queue.OutcomeDefer},
		"a seat this node holds":           {handle: "swe", err: refused("swe"), want: queue.OutcomeNak},
		"the store failed":                 {handle: "nobody", err: errors.New("store unavailable"), want: queue.OutcomeNak},
		"settled":                          {handle: "nobody", err: nil, want: queue.OutcomeAck},
	} {
		t.Run(name, func(t *testing.T) {
			if got := e.seatControlOutcome(tc.handle, tc.err).Outcome; got != tc.want {
				t.Errorf("seatControlOutcome(%s, %v) = %s, want %s", tc.handle, tc.err, got, tc.want)
			}
		})
	}
}
