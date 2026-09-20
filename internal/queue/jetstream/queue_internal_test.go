package jetstream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A FAILING MESSAGE BACKS OFF, and the budget outlives the outage.
//
// A flat spacing spent every one of a message's 25 attempts inside half a
// minute, so an LLM credential benched for its cooldown, a vendor's
// rate-limit window or a database restarting all dead-lettered work that the
// next attempt would have handled, while sending the struggling dependency 25
// requests a second apart on the way there.
func TestAFailingMessageBacksOffToACeiling(t *testing.T) {
	t.Parallel()
	q := &Queue{cfg: Config{NakDelay: time.Second, NakCeiling: 8 * time.Second}}

	// THE FIRST FAILURE IS STILL FAST. A blip must not cost a seat ten
	// minutes of silence, which is the other half of this decision.
	if got := q.nakBackoff(1); got != time.Second {
		t.Errorf("first redelivery waits %v, want the base delay", got)
	}
	for n, want := range map[uint64]time.Duration{
		2: 2 * time.Second,
		3: 4 * time.Second,
		4: 8 * time.Second,
		5: 8 * time.Second,
	} {
		if got := q.nakBackoff(n); got != want {
			t.Errorf("delivery %d waits %v, want %v", n, got, want)
		}
	}

	// A DELIVERY COUNT IS A NUMBER OFF THE WIRE. Shifting a duration by
	// it is undefined past 63 and negative well before that, and the
	// answer to a nonsense count is the longest wait rather than an
	// immediate redelivery, which is the failure this whole change is
	// about.
	for _, n := range []uint64{0, 64, 1 << 40} {
		if got := q.nakBackoff(n); got <= 0 || got > 8*time.Second {
			t.Errorf("delivery %d waits %v, want a bounded positive wait", n, got)
		}
	}

	// A CEILING UNDER THE BASE IS THE CEILING, not a delay that ignores it.
	tight := &Queue{cfg: Config{NakDelay: time.Minute, NakCeiling: time.Second}}
	if got := tight.nakBackoff(1); got != time.Second {
		t.Errorf("a ceiling below the base gives %v, want the ceiling", got)
	}

	// AND THE SHIPPED DEFAULTS SPAN THE OUTAGE THEY EXIST FOR: a message's
	// whole budget must outlast a benched credential's auth cooldown, which
	// is the failure that used to consume it in 25 seconds.
	var total time.Duration
	shipped := &Queue{}
	for n := uint64(1); n <= uint64(maxDeliver); n++ {
		total += shipped.nakBackoff(n)
	}
	if total < 5*time.Minute {
		t.Errorf("the default budget spans %v, which is shorter than the "+
			"auth cooldown a benched provider credential serves", total)
	}
}

// A HANDOFF IS NOT A FAILURE, and the backoff must not treat it as one.
//
// The wait doubled per DELIVERY, and this package's own budget comment says
// what a delivery counts: "poison, node-death AND HANDOFF — the last because
// a deferred delivery returns via Nak and that increments the count". Every
// healthy return goes through the same counter — a lease moving, a hold or a
// pause landing between the fetch and the dispatch — so a message handed back
// five times for nobody's fault met its first genuine failure already five
// steps up the curve, which at the shipped values is straight at the ceiling.
// "THE FIRST FAILURE IS STILL FAST" is the other half of the decision above,
// and it was not true.
func TestAHandoffDoesNotAgeAMessagesBackoff(t *testing.T) {
	t.Parallel()
	a := &attachment{}

	// Five handoffs of sequence 7: nothing here failed, so nothing is
	// recorded against it.
	const seq = 7
	if got := a.failed(seq); got != 1 {
		t.Fatalf("the first failure counted as %d", got)
	}
	a.settled(seq)

	// After the message is settled its first failure is a first failure
	// again, which is what a redelivery landing on a fresh attachment
	// gets.
	if got := a.failed(seq); got != 1 {
		t.Errorf("a settled message carried %d failures into its next life", got)
	}
}

// AND A REAL SEQUENCE OF FAILURES STILL AGES, or the fix above would be a way
// of retrying a poisoned message at full speed for ever.
func TestRepeatedFailuresOfOneMessageAge(t *testing.T) {
	t.Parallel()
	a := &attachment{}
	for want := uint64(1); want <= 4; want++ {
		if got := a.failed(11); got != want {
			t.Fatalf("failure %d counted as %d", want, got)
		}
	}
	// AND THEY ARE PER MESSAGE. One seat failing on one message must not
	// slow the next message down.
	if got := a.failed(12); got != 1 {
		t.Errorf("a different message started at %d", got)
	}
}

// NOTHING IS REMEMBERED FOR A MESSAGE THAT WILL NOT COME BACK, or the map
// grows for the life of the seat.
func TestASettledMessageIsForgotten(t *testing.T) {
	t.Parallel()
	a := &attachment{}
	a.failed(1)
	a.failed(2)
	a.settled(1)
	a.settled(2)

	a.failuresMu.Lock()
	defer a.failuresMu.Unlock()
	if len(a.failures) != 0 {
		t.Errorf("the failure map holds %d settled message(s)", len(a.failures))
	}
}

// AND A BATCH THAT ACKS AFTER FAILING IS FORGOTTEN TOO, or the map grows for
// the life of the attachment on the path every seat inbox actually runs.
//
// [attachment.apply] settles in its ack branch for exactly this reason, and
// the batch path did not — so a partition that failed once and succeeded on
// its redelivery left its sequence behind for ever. It is ordinary operation
// rather than a rarity: a coding run's answer that this node could not hand
// over is NAKed and acked on a later pass, which is precisely the
// NAK-then-ack shape, so every seat that ever hands an answer back leaked one
// entry per delivery.
//
// Driven through a real partition rather than asserted on the verbs, because
// the verbs were already right: what was missing was the CALL.
func TestABatchThatAcksAfterFailingIsForgotten(t *testing.T) {
	t.Parallel()
	q := openForTest(t, Config{})
	ctx := t.Context()
	topic, group := topics.AgentInbox("erin"), topics.AgentInboxGroup("erin")
	if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}

	var calls int // the consume loop is one goroutine, so this is its own
	observed := make(chan int, 4)
	if err := q.SubscribeBatch(ctx, topic, group,
		func(context.Context, []*events.Event) queue.Result {
			calls++
			if calls == 1 {
				return queue.Nak(errors.New("the seat is still owed this answer"))
			}
			// READ FROM INSIDE THE HANDLER, the one moment the order
			// is guaranteed: the NAK has been applied and this
			// partition's ack has not. It is what stops the case
			// passing vacuously on a map that was never written.
			observed <- failureEntries(q, topic, group)
			return queue.Ack()
		},
		func(*events.Event) string { return "one-conversation" },
		queue.DefaultBatchOptions(),
	); err != nil {
		t.Fatalf("SubscribeBatch: %v", err)
	}

	if err := q.Publish(ctx, topic, ev(1)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case held := <-observed:
		if held != 1 {
			t.Fatalf("the failure map held %d entries when the redelivery ran, "+
				"want the 1 its NAK recorded — this case never drove the hazard", held)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the redelivery never reached the handler")
	}

	deadline := time.Now().Add(10 * time.Second)
	for failureEntries(q, topic, group) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the failure map still holds %d settled message(s) after the "+
				"partition acked", failureEntries(q, topic, group))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// failureEntries counts what this pair's attachments remember about messages
// that have failed.
func failureEntries(q *Queue, topic, group string) int {
	total := 0
	for _, a := range q.lookup(topic, group) {
		a.failuresMu.Lock()
		total += len(a.failures)
		a.failuresMu.Unlock()
	}
	return total
}
