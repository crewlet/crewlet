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
// The slide this guards against needed an arrival in every fetch past the
// window's end — the drain used to end only on a fetch that brought nothing —
// and that is the broker's timing to give: a case publishing to a real one
// cannot promise an arrival lands in each fetch, so a runner slow to publish
// passed the slide (TestTheLingerWindowDoesNotSlideWithArrivals says how).
// Held here on the drain itself, over a scripted consumer whose every pull
// brings one message and a quarter of the window by the case's clock: the
// window opens with the first message in hand, and the fourth fetch after it
// returns at the window's end, so the drain holds five.
//
// Mutation: end the window only on a fetch that brought nothing, or move its
// end to each arrival, and the drain runs on to its cap; give it no window at
// all, and it ends at its first fetch after the one that opened it; end it on
// the wall clock, whatever clock it was handed, and no fetch ever reaches the
// end.
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
