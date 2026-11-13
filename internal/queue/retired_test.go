package queue_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A SUBSCRIPTION'S RETIRED COPY IS KEPT BY THE RETURN THAT SPENT ITS BUDGET: a
// failure's with the pair's poison, and a pass's or a hand-back's with its
// unhandled deliveries ([queue.RetiredTo]).
//
// A reader that replays a group's unhandled deliveries handles everything that
// subject holds as a delivery, and never reads the poison subject: a pass's
// copy kept with the poison is a wake nobody replays, and a failure's kept
// with the unhandled deliveries is poison handled again at every start. Both
// backends keep a copy where this answers, so the answer is held here once,
// and queuetest holds each backend to keeping where it says.
//
// Mutation: answer the poison subject for a pass, and a passed copy is kept
// with the poison; answer the unhandled subject for a Nak, and a failure's is
// replayed.
func TestARetiredCopyIsKeptByTheReturnThatSpentItsBudget(t *testing.T) {
	t.Parallel()
	const topic, group = "crewlet.notifications.inbound", "notify-inbound"
	for spent, want := range map[queue.Outcome]string{
		queue.OutcomeNak:   topics.DeadLetter(topic, group),
		queue.OutcomeDefer: topics.Unhandled(topic, group),
		queue.OutcomePass:  topics.Unhandled(topic, group),
	} {
		if got := queue.RetiredTo(topic, group, spent); got != want {
			t.Errorf("a copy whose budget a %s spent is kept on %s, want %s", spent, got, want)
		}
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
