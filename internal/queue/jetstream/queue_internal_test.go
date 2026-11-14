package jetstream

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// A FAILING MESSAGE BACKS OFF, and the budget outlives the outage.
//
// A flat spacing spent every one of a message's 25 attempts inside half a
// minute, so an LLM entry sitting out a spent plan, a vendor's rate-limit
// window or a database restarting all dead-lettered work that the
// next attempt would have handled, while sending the struggling dependency 25
// requests a second apart on the way there.
func TestAFailingMessageBacksOffToACeiling(t *testing.T) {
	t.Parallel()
	q := &Queue{cfg: Config{NakDelay: time.Second, NakCeiling: 8 * time.Second}}

	// THE FIRST FAILURE IS STILL FAST. A blip must not cost a seat ten
	// minutes of silence, which is the other half of this decision.
	if got := q.nakSpacing(1); got != time.Second {
		t.Errorf("first redelivery waits %v, want the base delay", got)
	}
	for n, want := range map[uint64]time.Duration{
		2: 2 * time.Second,
		3: 4 * time.Second,
		4: 8 * time.Second,
		5: 8 * time.Second,
	} {
		if got := q.nakSpacing(n); got != want {
			t.Errorf("delivery %d waits %v, want %v", n, got, want)
		}
	}

	// A DELIVERY COUNT IS A NUMBER OFF THE WIRE. Shifting a duration by
	// it is undefined past 63 and negative well before that, and the
	// answer to a nonsense count is the longest wait rather than an
	// immediate redelivery, which is the failure this whole change is
	// about.
	for _, n := range []uint64{0, 64, 1 << 40} {
		if got := q.nakSpacing(n); got <= 0 || got > 8*time.Second {
			t.Errorf("delivery %d waits %v, want a bounded positive wait", n, got)
		}
	}

	// A CEILING UNDER THE BASE IS THE CEILING, not a delay that ignores it.
	tight := &Queue{cfg: Config{NakDelay: time.Minute, NakCeiling: time.Second}}
	if got := tight.nakSpacing(1); got != time.Second {
		t.Errorf("a ceiling below the base gives %v, want the ceiling", got)
	}

	// AND THE SHIPPED DEFAULTS SPAN THE OUTAGE THEY EXIST FOR: a message's
	// whole budget must outlast the minute an LLM entry sits out after its
	// harness reports a spent plan with no reset instant
	// (internal/providers/llm/harness, bench.go) several times over — the
	// failure flat spacing consumed the budget on in 25 seconds.
	var total time.Duration
	shipped := &Queue{}
	for n := uint64(1); n <= uint64(maxDeliver); n++ {
		total += shipped.nakSpacing(n)
	}
	if total < 5*time.Minute {
		t.Errorf("the default budget spans %v, which does not outlast an LLM "+
			"entry's one-minute sit-out several times over", total)
	}
}

// A REDELIVERY IS SPREAD AROUND ITS SPACING, never past a fifth either way, so
// seats whose turns failed together do not all come back on one second.
func TestARedeliveryIsSpreadAroundItsSpacing(t *testing.T) {
	t.Parallel()
	q := &Queue{cfg: Config{NakDelay: time.Second, NakCeiling: 8 * time.Second}}
	spacing := q.nakSpacing(3)
	seen := map[time.Duration]bool{}
	for range 200 {
		got := q.nakBackoff(3)
		seen[got] = true
		if low, high := spacing-spacing/5, spacing+spacing/5; got < low || got > high {
			t.Fatalf("a redelivery spaced %v waits %v, outside [%v, %v]", spacing, got, low, high)
		}
	}
	if len(seen) < 2 {
		t.Errorf("200 redeliveries spaced %v all waited the same", spacing)
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
	if got, _ := a.failed(seq, true); got != 1 {
		t.Fatalf("the first failure counted as %d", got)
	}
	a.settled(seq)

	// After the message is settled its first failure is a first failure
	// again, which is what a redelivery landing on a fresh attachment
	// gets.
	if got, _ := a.failed(seq, true); got != 1 {
		t.Errorf("a settled message carried %d failures into its next life", got)
	}
}

// AND A REAL SEQUENCE OF FAILURES STILL AGES, or the fix above would be a way
// of retrying a poisoned message at full speed for ever.
func TestRepeatedFailuresOfOneMessageAge(t *testing.T) {
	t.Parallel()
	a := &attachment{}
	for want := uint64(1); want <= 4; want++ {
		if got, _ := a.failed(11, want%2 == 0); got != want {
			t.Fatalf("failure %d counted as %d", want, got)
		}
	}
	// AND THEY ARE PER MESSAGE. One seat failing on one message must not
	// slow the next message down.
	if got, _ := a.failed(12, true); got != 1 {
		t.Errorf("a different message started at %d", got)
	}
}

// NOTHING IS REMEMBERED FOR A MESSAGE THAT WILL NOT COME BACK, or the map
// grows for the life of the seat.
func TestASettledMessageIsForgotten(t *testing.T) {
	t.Parallel()
	a := &attachment{}
	a.failed(1, true)
	a.failed(2, false)
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
// rather than a rarity: an answer to a parked executor that this node could
// not hand over is NAKed and acked on a later pass, which is precisely the
// NAK-then-ack shape, so every seat that ever hands an answer back leaked one
// entry per delivery.
//
// Driven through a real partition rather than asserted on the verbs, because
// the verbs were already right: what was missing was the CALL.
func TestABatchThatAcksAfterFailingIsForgotten(t *testing.T) {
	t.Parallel()
	q := openForTest(t, Config{})
	ctx := t.Context()
	topic, group := seatInbox("erin"), seatGroup("erin")
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
	case <-time.After(20 * time.Second): //fixedwait:liveness
		t.Fatal("the redelivery never reached the handler")
	}

	deadline := time.Now().Add(10 * time.Second)
	for failureEntries(q, topic, group) != 0 {
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			t.Fatalf("the failure map still holds %d settled message(s) after the "+
				"partition acked", failureEntries(q, topic, group))
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
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

// A FETCH EXPIRES AFTER ITS WAIT, AND A STOP ENDS IT AT ONCE — the two halves
// of [attachment.fetch], each of which a client upgrade could move without a
// word.
//
// The first: a fetch on a consumer with nothing for it ends when the broker
// expires the request, and that must be the wait asked for — not a tenth early,
// which is what the client makes of a context's deadline unadjusted, and not
// the client's thirty-second default, which is what a context with no deadline
// would leave the request at. The second: a stop does not wait for an idle
// fetch to run out. It used to — a loop parked in a fetch noticed its
// cancellation only when the fetch ended, so every stop of a queue holding an
// idle attachment spent the whole stop grace and then closed the connection
// under the loop anyway.
//
// NEITHER IS DECIDED ON THE MACHINE'S CLOCK. The broker ends an idle request at
// the expiry the request carries, by its own timer, so the first half is that
// expiry, read where the vendored client writes it; it was a measured fetch
// held to a second past its wait, which a runner that lost a second failed,
// and which a fetch never sent at all passed. The second half needs the loop
// in its fetch before the stop lands, and waits for the fetch's request to
// reach the broker; it slept a tenth of a second for that, and a loop
// scheduled later than the stop's grace failed a healthy build while one
// scheduled after the stop passed with the stop reaching no fetch at all.
//
// Mutation: drop expiryHeldBack's share from the deadline and the request
// expires a tenth early; drop the first FetchContext and it expires at the
// client's thirty seconds; let a stop leave the fetch alone (no abandon) and
// the loop is still in it when Stop returns.
func TestAFetchExpiresAfterItsWaitAndAStopEndsItAtOnce(t *testing.T) {
	t.Parallel()

	t.Run("an idle fetch ends at its wait", func(t *testing.T) {
		t.Parallel()
		// IN A BUBBLE, so the instant the fetch takes its deadline and the
		// instant the client reads what is left of it are one instant: on
		// the machine's clock the expiry is short of the wait by whatever
		// a loaded runner spent between the two.
		inBubble(t, func(t *testing.T) {
			const wait = 500 * time.Millisecond
			cons := &expiryRecorder{}
			a := &attachment{cons: cons, fetching: t.Context()}
			if err := a.fetch(1, wait, func(jetstream.Msg) {
				t.Error("an empty mailbox handed over a message")
			}); err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if len(cons.expiries) != 1 || cons.expiries[0] != wait {
				t.Errorf("a fetch asked to wait %v asked the broker for %v", wait, cons.expiries)
			}
		})
	})

	t.Run("a stop ends an idle fetch at once", func(t *testing.T) {
		t.Parallel()
		// A POLL OF A MINUTE, so a fetch the stop did not reach is still
		// outstanding long after the stop's own grace.
		q := newQueueWith(t, Config{FetchWait: time.Minute})
		topic, group := seatInbox("stopping"), seatGroup("stopping")
		inFetch := fetchSent(t, q, topic, group)
		if err := q.Subscribe(t.Context(), topic, group,
			func(context.Context, *events.Event) queue.Result { return queue.Ack() }); err != nil {
			t.Fatal(err)
		}
		atts := q.lookup(topic, group)
		if len(atts) != 1 {
			t.Fatalf("%d attachments, want 1", len(atts))
		}
		// THE LOOP PARKED IN ITS FETCH, waited for rather than slept
		// toward: once its request is at the broker, only the stop can
		// end that fetch inside the minute, however late the runner
		// scheduled the loop.
		inFetch()
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Fatal(err)
		}
		select {
		case <-atts[0].done:
		default:
			t.Fatal("the consume loop was still parked in its fetch when Stop " +
				"returned, so the stop waited out its grace for a loop it then " +
				"abandoned")
		}
	})
}

// expiryRecorder is a consumer with nothing for a fetch: it sends no request,
// and records the expiry the vendored client would have put on each one.
//
// THE CLIENT'S OWN OPTIONS, applied as its Fetch applies them. A fetch's
// options are functions over the client's unexported request, so the request
// is built by reflection, at the client's default expiry as Fetch starts it,
// and each option is applied in order. What is read back is the client's own
// arithmetic on the deadline it was handed: an upgrade that moves it moves this
// reading, and one that reshapes the options fails here by name.
type expiryRecorder struct {
	jetstream.Consumer
	expiries []time.Duration
}

func (r *expiryRecorder) Fetch(batch int, opts ...jetstream.FetchOpt) (jetstream.MessageBatch, error) {
	var req reflect.Value
	for _, opt := range opts {
		apply := reflect.ValueOf(opt)
		if !req.IsValid() {
			req = reflect.New(apply.Type().In(0).Elem())
			req.Elem().FieldByName("Batch").SetInt(int64(batch))
			req.Elem().FieldByName("Expires").SetInt(int64(jetstream.DefaultExpires))
		}
		if err, _ := apply.Call([]reflect.Value{req})[0].Interface().(error); err != nil {
			return nil, err
		}
	}
	if !req.IsValid() {
		r.expiries = append(r.expiries, jetstream.DefaultExpires)
	} else {
		r.expiries = append(r.expiries, time.Duration(req.Elem().FieldByName("Expires").Int()))
	}
	nothing := make(chan jetstream.Msg)
	close(nothing)
	return emptyBatch(nothing), nil
}

// emptyBatch is a fetch that came back with nothing.
type emptyBatch <-chan jetstream.Msg

func (b emptyBatch) Messages() <-chan jetstream.Msg { return b }

func (emptyBatch) Error() error { return nil }

// fetchSent subscribes to the requests the fetches of topic's consumer for
// group send the broker, and returns the wait for the first: the moment a
// consume loop's fetch is at the broker, with the loop parked in it.
//
// ON THE SUBJECT THE BROKER'S JETSTREAM SERVES, which a domain's own form is
// mapped to before any subscriber reads it, and ON THE QUEUE'S OWN CONNECTION,
// whose protocol the broker reads in order: subscribed before the queue
// fetches, the subscription is in place before any fetch's request arrives.
func fetchSent(t *testing.T, q *Queue, topic, group string) func() {
	t.Helper()
	subject := "$JS.API.CONSUMER.MSG.NEXT." + q.mustStream(t, topic) + "." + consumerName(topic, group)
	sub, err := q.nc.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", subject, err)
	}
	return func() {
		t.Helper()
		// LIVENESS: a loop that never fetches never sends a request, and
		// the bound only ends that case's wait; a loop that fetches late
		// is waited for.
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		if _, err := sub.NextMsgWithContext(ctx); err != nil {
			t.Fatalf("no fetch reached the broker on %s: %v", subject, err)
		}
		if err := sub.Unsubscribe(); err != nil {
			t.Fatalf("unsubscribe from %s: %v", subject, err)
		}
	}
}

// A FETCH WHOSE WINDOW LAPSED BEFORE IT WAS SENT IS AN EMPTY ONE, and the
// message it did not ask for is still there for the next.
//
// The client refuses a fetch context whose deadline is already past when it
// applies the option, so a consume loop descheduled between taking its
// deadline and sending the request for longer than the window was handed an
// "invalid option" — logged as fetch_failed, followed by a whole poll of
// sleep, for a scheduler hiccup. A window of nothing has lapsed by the time
// any request could be sent, which is the same refusal without needing a
// scheduler to produce it.
func TestAFetchWhoseWindowLapsedBeforeItWasSentIsAnEmptyOne(t *testing.T) {
	t.Parallel()
	q := newQueue(t)
	topic, group := seatInbox("lapsed"), seatGroup("lapsed")
	if _, err := q.EnsureSubscription(t.Context(), topic, group); err != nil {
		t.Fatal(err)
	}
	if err := q.Publish(t.Context(), topic, ev(1)); err != nil {
		t.Fatal(err)
	}
	cons, err := q.js.Consumer(t.Context(), q.mustStream(t, topic), consumerName(topic, group))
	if err != nil {
		t.Fatal(err)
	}
	a := &attachment{cons: cons, fetching: t.Context()}
	if err := a.fetch(1, 0, func(jetstream.Msg) {
		t.Error("a fetch that was never sent handed over a message")
	}); err != nil {
		t.Fatalf("a fetch whose window lapsed before it was sent answered %v, "+
			"want an empty fetch — the loop logs and sleeps a poll on an error", err)
	}
	var got int
	if err := a.fetch(1, 5*time.Second, func(msg jetstream.Msg) {
		got++
		_ = msg.Ack()
	}); err != nil {
		t.Fatalf("the next fetch: %v", err)
	}
	if got != 1 {
		t.Errorf("the next fetch took %d message(s), want the 1 the lapsed one "+
			"never asked for", got)
	}
}
