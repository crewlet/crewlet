package jetstream

import (
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/queuetest"
)

// THE WINDOW IS THE BOUND, HOWEVER FULL THE FETCHES INSIDE IT COME BACK: a
// drain ends at the first fetch that returns with its window's end reached,
// though every fetch it made brought mail.
//
// Held on the drain itself, since a real broker cannot promise an arrival in
// each fetch (TestTheLingerWindowDoesNotSlideWithArrivals). Every scripted
// pull brings one message and a quarter of the window by the case's clock, so
// the fourth fetch after the first message reaches the end: five held.
//
// Mutation: end the window only on an empty fetch, move its end to each
// arrival, or end it on the wall clock, and the drain runs to its cap; give it
// no window, and it ends at the fetch after the first.
func TestTheLingerWindowEndsAtItsDeadlineWhateverItsFetchesBring(t *testing.T) {
	t.Parallel()
	const (
		window = time.Minute
		// Well under spunPulls, which ends the case from inside a drain
		// that never stops asking.
		maxBatch = 16
	)
	clock := &queuetest.Clock{}
	cons := &scriptedConsumer{t: t, clock: clock, step: window / 4,
		answers: []pullAnswer{{delivered: []jetstream.Msg{eventMsg{}}}}}
	a := &attachment{
		q:        &Queue{cfg: Config{clock: clock}},
		cons:     cons,
		log:      slog.New(slog.DiscardHandler),
		fetching: t.Context(),
	}
	batch := a.drain(t.Context(), queue.NewBatchOptions(window.Seconds(), maxBatch))
	if len(batch) != 5 || len(cons.fetches) != 5 {
		t.Fatalf("a drain whose every pull brought a message and a quarter of its window "+
			"held %d message(s) from %d pull(s), want 5 from 5: the first and one from each "+
			"pull up to the window's end (%d is the cap, which a drain runs on to when its "+
			"window slid, or when it ends the window on a clock other than the one it was "+
			"handed)", len(batch), len(cons.fetches), maxBatch)
	}
}
