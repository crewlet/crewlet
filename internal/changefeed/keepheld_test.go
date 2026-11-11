package changefeed_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/queue"
)

// slowKeeper keeps a dead letter only once takes has passed — a keep waiting
// as long as a large frame takes to cross a slow link — and counts the keeps
// it was asked for. It holds nothing a replay would read.
type slowKeeper struct {
	takes time.Duration

	mu    sync.Mutex
	keeps int
}

func (k *slowKeeper) KeepDeadLetter(context.Context, string, []byte, []byte) error {
	k.mu.Lock()
	k.keeps++
	k.mu.Unlock()
	time.Sleep(k.takes) //fixedwait:simulated a keep waiting while its frame crosses a slow link
	return nil
}

func (k *slowKeeper) DeadLettersOn(context.Context, string) ([]queue.Kept, error) {
	return nil, nil
}

func (k *slowKeeper) DeadLetterFrame(context.Context, string, uint64) ([]byte, bool, error) {
	return nil, false, nil
}

func (k *slowKeeper) DropDeadLetter(context.Context, string, uint64) error { return nil }

func (k *slowKeeper) kept() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.keeps
}

// windowBroker delivers a record to a feed as the feed's group does: a
// delivery that goes its acknowledgement window without an ack, a nak or a
// progress signal is handed back — delivered again, its count one higher —
// and every delivery and ack is counted. It delivers a record at most limit
// times, so a case whose feed lets its deliveries lapse still ends.
type windowBroker struct {
	feed   *changefeed.Feed
	window time.Duration
	limit  int

	mu         sync.Mutex
	deliveries int
	acks       int
	handlers   sync.WaitGroup
}

// deliver hands rec to the feed as one delivery, settled on this goroutine,
// and hands it back on a goroutine of its own if its window lapses first.
func (b *windowBroker) deliver(ctx context.Context, rec changefeed.Record) {
	b.mu.Lock()
	b.deliveries++
	b.mu.Unlock()
	var (
		mu    sync.Mutex
		over  bool
		lapse *time.Timer
	)
	settle := func() bool {
		mu.Lock()
		defer mu.Unlock()
		if over {
			return false
		}
		over = true
		lapse.Stop()
		return true
	}
	mu.Lock()
	lapse = time.AfterFunc(b.window, func() { //fixedwait:simulated the group's acknowledgement window, which hands a delivery back
		mu.Lock()
		handedBack := !over
		over = true
		mu.Unlock()
		b.mu.Lock()
		again := handedBack && b.deliveries < b.limit
		b.mu.Unlock()
		if again {
			next := rec
			next.Delivered++
			b.handlers.Go(func() { b.deliver(ctx, next) })
		}
	})
	mu.Unlock()
	changefeed.Handle(ctx, b.feed, &changefeed.Message{
		Record: rec, Frame: aFrame,
		Deferred: &changefeed.Deferral{Reason: changefeed.DeferNewerEnvelope,
			Err: errors.New("an envelope a newer build writes")},
		Ack: func() error {
			if settle() {
				b.mu.Lock()
				b.acks++
				b.mu.Unlock()
			}
			return nil
		},
		Nak: func(time.Duration) error {
			settle()
			return nil
		},
		Progress: func() error {
			mu.Lock()
			defer mu.Unlock()
			if !over {
				lapse.Reset(b.window)
			}
			return nil
		},
		ProgressEvery: b.window / 3,
	})
}

func (b *windowBroker) counts() (deliveries, acks int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deliveries, b.acks
}

// A KEEP LONGER THAN ITS DELIVERY'S ACKNOWLEDGEMENT WINDOW HOLDS THE DELIVERY,
// and the record is kept once.
//
// A dead letter's keep waits as long as its frame takes to cross the node's
// link to the broker, and the feed acks the record only once the keep returns:
// a keep that outlasted the window had its record handed back mid-keep, and
// the redelivery, still past the age, was kept again — two entries of one
// record in the dead letters, and a second copy across the link. So while a
// keep runs the feed sends the delivery's progress signal at the cadence the
// delivery states. Here a broker of the case's own hands the record back once
// its window passes with no word, and the keep takes two windows: delivered
// once, kept once, acked once. In a bubble, so the windows are the clock's and
// not a loaded runner's.
//
// Mutation: keep without sending the delivery's progress signal, and the
// record is handed back mid-keep and kept twice.
func TestAKeepLongerThanItsAckWindowHoldsItsDelivery(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const window = 30 * time.Second
		keeper := &slowKeeper{takes: 2 * window}
		clock := newClock()
		feed, err := changefeed.New(changefeed.Options{
			Opener: newEstate(), Publisher: &capture{}, Claims: newClaims(), Translator: newProbe(),
			DeadLetters: keeper, Now: clock.now,
		})
		if err != nil {
			t.Fatalf("new feed: %v", err)
		}
		broker := &windowBroker{feed: feed, window: window, limit: 2}
		broker.deliver(t.Context(), aRecord(clock.now().Add(-changefeed.DeadLetterAge-time.Minute), 1))
		broker.handlers.Wait()
		synctest.Wait()
		deliveries, acks := broker.counts()
		if kept := keeper.kept(); kept != 1 || deliveries != 1 || acks != 1 {
			t.Errorf("a keep of two windows: delivered %d times, kept %d and acked %d, want the "+
				"one delivery kept once and acked once", deliveries, kept, acks)
		}
	})
}
