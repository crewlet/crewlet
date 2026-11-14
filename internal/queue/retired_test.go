package queue_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A SUBSCRIPTION'S RETIRED COPY IS KEPT BY WHETHER THE MESSAGE FAILED AS
// ITSELF: with the pair's poison if one of its deliveries did, and with its
// unhandled deliveries otherwise ([queue.RetiredTo]).
//
// A reader that replays a group's unhandled deliveries handles everything that
// subject holds as a delivery, and never reads the poison subject: a pass's
// copy kept with the poison is a wake nobody replays, and a failure's kept
// with the unhandled deliveries is poison handled again at every start. Both
// backends keep a copy where this answers, so the answer is held here once,
// and queuetest holds each backend to keeping where it says.
//
// Mutation: answer the poison subject for a message that never failed, and a
// passed copy is kept with the poison; answer the unhandled subject for one
// that did, and a failure's is replayed.
func TestARetiredCopyIsKeptByWhetherTheMessageFailed(t *testing.T) {
	t.Parallel()
	const topic, group = "crewlet.notifications.inbound", "notify-inbound"
	if got, want := queue.RetiredTo(topic, group, true), topics.DeadLetter(topic, group); got != want {
		t.Errorf("a message that failed as itself is kept on %s, want the poison's %s", got, want)
	}
	if got, want := queue.RetiredTo(topic, group, false), topics.Unhandled(topic, group); got != want {
		t.Errorf("a message that never failed as itself is kept on %s, want the unhandled "+
			"deliveries' %s", got, want)
	}
}

// A NODE'S OWN FAILURE IS NO FAILURE OF THE MESSAGE, and a Nak is.
//
// A handler whose node could not reach its broker, or whose stop ended it,
// answers [queue.NakNode]: returned as any failure is, and never the
// message's, so a message whose budget it spends is kept with the unhandled
// deliveries ([queue.Result.FailsMessage]). Every other return is no failure
// at all.
//
// Mutation: count a node's own failure as the message's, and its row is red.
func TestANodesOwnFailureIsNoFailureOfTheMessage(t *testing.T) {
	t.Parallel()
	boom := errors.New("the broker did not answer")
	for name, tc := range map[string]struct {
		res   queue.Result
		fails bool
	}{
		"a failure":            {queue.Nak(boom), true},
		"the node's own":       {queue.NakNode(boom), false},
		"a deferral":           {queue.Defer(queue.DeferShedding, "shedding"), false},
		"a pass":               {queue.Pass("newer_record", boom), false},
		"an acknowledgement":   {queue.Ack(), false},
		"the node's, outright": {queue.Result{Outcome: queue.OutcomeNak, NodeFault: true}, false},
	} {
		if got := tc.res.FailsMessage(); got != tc.fails {
			t.Errorf("%s fails the message: %v, want %v", name, got, tc.fails)
		}
	}
	if !queue.NakNode(boom).NodeFault || queue.NakNode(boom).Outcome != queue.OutcomeNak {
		t.Error("NakNode is not a Nak marked as the node's own")
	}
}

// A DEAD LETTER FITS UP TO THE CEILING LESS ITS FRAMING, AND NOT A BYTE PAST
// IT, because [queue.LetterFits] is the one rule every keep is held to on
// every backend: a twin that kept one byte more than the broker carries would
// certify a keep the broker then refuses.
//
// Mutation: compare with `<` rather than `<=`, and a letter exactly at the
// ceiling does not fit.
func TestALetterFitsUpToTheCeilingLessItsFraming(t *testing.T) {
	t.Parallel()
	header := []byte(`{"v":1}`)
	room := queue.MaxPayloadBytes - queue.LetterFraming - len(header)
	if !queue.LetterFits(header, make([]byte, room)) {
		t.Errorf("a %d-byte body beside a %d-byte header does not fit, though the two "+
			"and the framing come to the ceiling exactly", room, len(header))
	}
	if queue.LetterFits(header, make([]byte, room+1)) {
		t.Errorf("a %d-byte body beside a %d-byte header fits, a byte past the ceiling",
			room+1, len(header))
	}
}
