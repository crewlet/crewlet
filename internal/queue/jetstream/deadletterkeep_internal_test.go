package jetstream

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// spentDelivery is a delivery of a fresh event on a's subscription at the last
// of its budget's deliveries, so whatever returns it next dead-letters it: the
// event, and the message carrying it.
func spentDelivery(t *testing.T, a *attachment) (*events.Event, *answeredMsg) {
	t.Helper()
	ev := &events.Event{ID: uuid.New(), Type: "external_notification", Source: "tracker"}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("encode the event: %v", err)
	}
	return ev, &answeredMsg{data: data, md: &jetstream.MsgMetadata{
		NumDelivered: uint64(budgetFor(a.q.cfg)), Sequence: jetstream.SequencePair{Stream: 7}}}
}

// keptOn is the one entry subject holds, failing the case unless the broker
// counts exactly one there and its body is data.
func keptOn(t *testing.T, q *Queue, under, subject string, data []byte) {
	t.Helper()
	marks, err := q.DeadLetterMarks(t.Context(), under)
	if err != nil {
		t.Fatalf("DeadLetterMarks: %v", err)
	}
	if got := marks.Counts[subject]; got != 1 {
		t.Fatalf("%s holds %d entries, want the one copy", subject, got)
	}
	listed, err := q.DeadLettersOn(t.Context(), subject)
	if err != nil || len(listed) != 1 {
		t.Fatalf("DeadLettersOn(%s) = %d entries, %v; want the one copy", subject, len(listed), err)
	}
	frame, held, err := q.DeadLetterFrame(t.Context(), subject, listed[0].Seq)
	if err != nil || !held || !bytes.Equal(frame, data) {
		t.Fatalf("the copy reads back held %v (%v), %d bytes, want the message's own %d",
			held, err, len(frame), len(data))
	}
}

// A DEAD LETTER IS KEPT THOUGH ITS LOOP IS STOPPING.
//
// A stop cancels the consume loop's context, and the copy of a message whose
// last delivery a stop met — a batch the loop handed back as the queue
// stopped, or a return made just before — was kept under that context: its
// write failed on the cancellation, with nothing wrong but the moment. A keep
// that fails leaves its message for the next delivery to keep
// (TestAKeepThatFailsIsMadeAgainByTheNextDelivery), but a stopping node's
// return of it may not reach the broker either, and the message then comes
// back only at the end of its acknowledgement window, half an hour on; so the
// stopping loop's keep is made, and lands.
//
// Mutation: keep the copy under the loop's own context, and a stopping loop's
// hand-back keeps nothing and terminates nothing.
func TestADeadLetterIsKeptThoughItsLoopIsStopping(t *testing.T) {
	t.Parallel()
	const topic, group = "stopping.letters", "grp"
	q := openForTest(t, Config{})
	a := &attachment{q: q, key: attachKey{topic: topic, group: group},
		log: slog.New(slog.DiscardHandler)}
	ev, msg := spentDelivery(t, a)

	stopping, stop := context.WithCancel(t.Context())
	stop()
	a.handBack(stopping, msg, ev, handBackStopping)

	if msg.termed != 1 {
		t.Errorf("the spent message was terminated %d times, want once", msg.termed)
	}
	keptOn(t, q, topics.DeadLettersUnder("stopping"), topics.Unhandled(topic, group), msg.data)
}

// A DRAIN WAITS FOR A RETURN'S DEAD LETTER, NOT ONLY FOR ITS HANDLER.
//
// A drain stops dispatching, waits for what is in flight and stops the queue
// ([queue.EventQueue.WaitForHandlers]). A delivery read as finished once its
// handler returned left the settling of what it returned outside that wait —
// for a return that spends the budget, a dead letter's keep, which crosses a
// link at its bytes' pace — and the stop that followed closed the connection
// under the keep. So a delivery is in flight until its return is settled: here
// the keep has been made and the message is being terminated, and the queue
// still counts it.
//
// Mutation: end the delivery's flight once its handler returns (endHandler
// before apply), and the queue counts nothing in flight while the spent
// message is terminated.
func TestADrainWaitsForAReturnsDeadLetter(t *testing.T) {
	t.Parallel()
	const topic, group = "settling.letters", "grp"
	q := openForTest(t, Config{})
	a := &attachment{q: q, key: attachKey{topic: topic, group: group},
		log: slog.New(slog.DiscardHandler)}
	_, msg := spentDelivery(t, a)
	terming, release := make(chan struct{}), make(chan struct{})
	msg.onTerm = func() {
		close(terming)
		<-release
	}

	dispatched := make(chan struct{})
	go func() {
		defer close(dispatched)
		a.dispatchOne(t.Context(), msg, func(context.Context, *events.Event) queue.Result {
			return queue.Pass("newer_record", nil)
		})
	}()
	<-terming
	if n := q.InFlightCount(); n != 1 {
		t.Errorf("%d deliveries in flight while the spent message is settled, want the one: "+
			"a drain would stop the queue under its dead letter's keep", n)
	}
	close(release)
	<-dispatched
	if n := q.InFlightCount(); n != 0 {
		t.Errorf("%d deliveries in flight once the spent message is settled, want none", n)
	}
	keptOn(t, q, topics.DeadLettersUnder("settling"), topics.Unhandled(topic, group), msg.data)
}

// refuseKeeps makes q's dead-letter stream refuse every keep, refuse being
// true — a message larger than a byte, which every keep is — or take them
// again: a keep the broker refuses, for nothing wrong with the message.
func refuseKeeps(t *testing.T, q *Queue, refuse bool) {
	t.Helper()
	st, err := q.js.Stream(t.Context(), streamDeadLetter)
	if err != nil {
		t.Fatalf("read %s: %v", streamDeadLetter, err)
	}
	cfg := st.CachedInfo().Config
	cfg.MaxMsgSize = -1
	if refuse {
		cfg.MaxMsgSize = 1
	}
	if _, err := q.js.UpdateStream(t.Context(), cfg); err != nil {
		t.Fatalf("update %s: %v", streamDeadLetter, err)
	}
}

// keptCount is how many entries subject holds, as the broker counts them.
func keptCount(t *testing.T, q *Queue, under, subject string) uint64 {
	t.Helper()
	marks, err := q.DeadLetterMarks(t.Context(), under)
	if err != nil {
		t.Fatalf("DeadLetterMarks: %v", err)
	}
	return marks.Counts[subject]
}

// A KEEP THAT FAILS IS MADE AGAIN BY THE MESSAGE'S NEXT DELIVERY.
//
// The consume loop retires a message at its budget's delivery by keeping its
// copy and terminating it, and a keep can fail for nothing wrong with the
// message: a stop that closes the connection under it, a node killed while it
// crosses, a dead-letter stream electing its leader — here, one refusing what
// it is sent. Terminated all the same, as it was, the message was gone with
// no copy; and with the broker's backstop at the budget it could not have been
// delivered again anyway. So the message goes back, the broker delivers it
// past its budget, and the keep its next delivery makes lands.
//
// Real on both ends: a subscription whose budget is one delivery, its handler
// passing every delivery, and the dead-letter stream refusing until the second
// delivery is in the handler's hands.
//
// Mutation: terminate the message once its keep fails, or hold the broker's
// MaxDeliver to the budget, and it is never delivered again.
func TestAKeepThatFailsIsMadeAgainByTheNextDelivery(t *testing.T) {
	t.Parallel()
	const topic, group = "refused.letters", "grp"
	q := openForTest(t, Config{MaxDeliver: 1})
	refuseKeeps(t, q, true)

	var deliveries atomic.Int32
	again, accepting := make(chan struct{}), make(chan struct{})
	if err := q.Subscribe(t.Context(), topic, group, func(context.Context, *events.Event) queue.Result {
		if deliveries.Add(1) == 2 {
			close(again)
			<-accepting
		}
		return queue.Pass("newer_record", nil)
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := q.Publish(t.Context(), topic, &events.Event{ID: uuid.New(), Type: "external_notification"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-again:
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("a message whose keep failed was never delivered again: its copy is lost")
	}
	refuseKeeps(t, q, false)
	close(accepting)

	under, unhandled := topics.DeadLettersUnder("refused"), topics.Unhandled(topic, group)
	bound, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	poll := time.NewTicker(5 * time.Millisecond) //fixedwait:liveness
	defer poll.Stop()
	for keptCount(t, q, under, unhandled) != 1 {
		select {
		case <-bound.Done():
			t.Fatalf("%s holds %d entries once the second delivery's keep could land, want the "+
				"one copy", unhandled, keptCount(t, q, under, unhandled))
		case <-poll.C:
		}
	}
}

// A KEEP MADE AGAIN OF A MESSAGE KEPT ALREADY IS STORED AS NOTHING.
//
// A keep can store its entry and lose its answer — the error is all its keeper
// hears — and the message then goes back for its next delivery to keep
// (TestAKeepThatFailsIsMadeAgainByTheNextDelivery). That keep names the
// message's own identity, its stream, sequence and stored instant (letterOf),
// so the dead-letter stream stores it as nothing: one entry, and the message
// terminated, however many keeps of it there were.
//
// Mutation: keep the copy with no identity, and the second keep is a second
// entry.
func TestAKeepMadeAgainOfAMessageKeptAlreadyIsStoredAsNothing(t *testing.T) {
	t.Parallel()
	const topic, group = "again.letters", "grp"
	q := openForTest(t, Config{})
	a := &attachment{q: q, key: attachKey{topic: topic, group: group},
		log: slog.New(slog.DiscardHandler)}
	ev, first := spentDelivery(t, a)
	first.md.Stream, first.md.Timestamp = "CREWLET_NOTIFICATIONS", time.Now().UTC()
	a.handBack(t.Context(), first, ev, "stopping")

	// THE SAME MESSAGE, ITS NEXT DELIVERY: the first keep's answer lost.
	md := *first.md
	md.NumDelivered++
	again := &answeredMsg{data: first.data, md: &md}
	a.handBack(t.Context(), again, ev, "stopping")

	if first.termed != 1 || again.termed != 1 {
		t.Errorf("the two deliveries were terminated %d and %d times, want once each",
			first.termed, again.termed)
	}
	keptOn(t, q, topics.DeadLettersUnder("again"), topics.Unhandled(topic, group), first.data)
}

// A KEEP THAT FAILS AT THE BROKER'S LAST DELIVERY LOSES THE COPY, SAID AS
// SUCH; ONE THAT FAILS BEFORE IT RETURNS THE MESSAGE.
//
// Past the budget the broker delivers a message keepRetries times more, so a
// failed keep goes back, spaced, for the next delivery to keep — and at the
// backstop's own delivery nothing follows, so the message is terminated, the
// copy lost.
//
// Mutation: terminate at every failed keep, and the delivery before the
// backstop is gone; return the backstop's own, and it is left for an
// acknowledgement window nothing follows.
func TestAFailedKeepReturnsTheMessageUntilTheBrokersLastDelivery(t *testing.T) {
	t.Parallel()
	const topic, group = "backstop.letters", "grp"
	q := openForTest(t, Config{})
	refuseKeeps(t, q, true)
	a := &attachment{q: q, key: attachKey{topic: topic, group: group},
		log: slog.New(slog.DiscardHandler)}
	ev, msg := spentDelivery(t, a)
	msg.md.NumDelivered = uint64(backstopFor(q.cfg)) - 1
	a.handBack(t.Context(), msg, ev, "stopping")
	if msg.termed != 0 || len(msg.delays) != 1 {
		t.Errorf("a failed keep before the broker's last delivery was answered with %d "+
			"terminations and spaced returns %v, want one spaced return", msg.termed, msg.delays)
	}

	_, last := spentDelivery(t, a)
	last.md.NumDelivered = uint64(backstopFor(q.cfg))
	a.handBack(t.Context(), last, ev, "stopping")
	if last.termed != 1 || len(last.delays) != 0 || last.naked != 0 {
		t.Errorf("a failed keep at the broker's last delivery was answered with %d "+
			"terminations, spaced returns %v and %d immediate ones, want it terminated",
			last.termed, last.delays, last.naked)
	}
}

// A BROKER'S BACKSTOP STANDS PAST THE BUDGET.
//
// The consume loop retires a message at its budget, never the broker; the
// broker's MaxDeliver is the backstop, and stands keepRetries deliveries past
// the budget so a keep that failed there is made again. Read off the consumer
// the broker holds.
//
// Mutation: set the consumer's MaxDeliver to the budget, and the consumer
// reports it.
func TestTheBrokersBackstopStandsPastTheBudget(t *testing.T) {
	t.Parallel()
	const topic, group = "backstop.consumer", "grp"
	q := openForTest(t, Config{MaxDeliver: 3})
	if _, err := q.EnsureSubscription(t.Context(), topic, group); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}
	stream, err := q.streamFor(t.Context(), topic)
	if err != nil {
		t.Fatalf("the stream of %s: %v", topic, err)
	}
	cons, err := q.js.Consumer(t.Context(), stream, consumerName(topic, group))
	if err != nil {
		t.Fatalf("the consumer of (%s, %s): %v", topic, group, err)
	}
	if got, want := cons.CachedInfo().Config.MaxDeliver, 3+keepRetries; got != want {
		t.Errorf("the broker delivers a message %d times, want %d: the budget's 3 and %d "+
			"more for a keep that failed", got, want, keepRetries)
	}
}

// A BACKEND'S OWN HAND-BACK IS IN FLIGHT UNTIL IT IS SETTLED, AND IS KEPT WITH
// THE UNHANDLED DELIVERIES.
//
// A delivery fetched for a consumer a hold met before its handler ran is handed
// back by the backend itself, on the single path (dispatchOne) and of a drain
// (handBackUndispatched). No handler runs, but the hand-back spends a delivery
// as every return does, and the one that spends the budget keeps a dead letter
// that crosses a link at its bytes' pace: outside the in-flight count, a drain
// read the queue as idle and stopped it under the keep. And it is no failure,
// so its copy is kept with the group's unhandled deliveries, where a reader
// that can handle it finds it, never with the poison.
//
// Mutation: hand back outside the in-flight count, on either path, and the
// queue counts nothing in flight while the spent message is settled; charge
// the backend's own hand-back as a failure, and its copy is kept with the
// poison.
func TestABackendsOwnHandBackIsInFlightUntilSettled(t *testing.T) {
	t.Parallel()
	for name, handBack := range map[string]func(a *attachment, msg *answeredMsg){
		"the single path": func(a *attachment, msg *answeredMsg) {
			a.dispatchOne(t.Context(), msg, func(context.Context, *events.Event) queue.Result {
				t.Error("a handler ran on a delivery a hold met")
				return queue.Ack()
			})
		},
		"a drain": func(a *attachment, msg *answeredMsg) {
			var ev events.Event
			if err := json.Unmarshal(msg.data, &ev); err != nil {
				t.Errorf("decode the delivery: %v", err)
			}
			a.handBackUndispatched(t.Context(), []delivery{{msg: msg, ev: &ev}}, a.blockedBy())
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const group = "grp"
			topic := "held.letters"
			q := openForTest(t, Config{})
			if err := q.PauseTopic(t.Context(), topic, group, "a person paused the seat"); err != nil {
				t.Fatalf("PauseTopic: %v", err)
			}
			a := &attachment{q: q, key: attachKey{topic: topic, group: group},
				log: slog.New(slog.DiscardHandler)}
			_, msg := spentDelivery(t, a)
			terming, release := make(chan struct{}), make(chan struct{})
			msg.onTerm = func() {
				close(terming)
				<-release
			}
			handedBack := make(chan struct{})
			go func() {
				defer close(handedBack)
				handBack(a, msg)
			}()
			<-terming
			if n := q.InFlightCount(); n != 1 {
				t.Errorf("%d deliveries in flight while the backend's own hand-back is settled, "+
					"want the one: a drain would stop the queue under its dead letter's keep", n)
			}
			close(release)
			<-handedBack
			if n := q.InFlightCount(); n != 0 {
				t.Errorf("%d deliveries in flight once the hand-back is settled, want none", n)
			}
			under := topics.DeadLettersUnder("held")
			keptOn(t, q, under, topics.Unhandled(topic, group), msg.data)
			if n := keptCount(t, q, under, topics.DeadLetter(topic, group)); n != 0 {
				t.Errorf("the pair's poison holds %d entries, want none: a hand-back is no failure", n)
			}
		})
	}
}

// A STOP WAITS FOR A DEAD LETTER BEING KEPT.
//
// A stop ends every consume loop, and a loop that meets the end holding a
// delivery hands it back; the one that spends its budget keeps a dead letter,
// a write that crosses a link at its bytes' pace. A stop that closed the
// connection a quarter of a second after ending the loops cut the keep under
// it. So the connection closes only once no keep is in flight: here a keep is
// settling, held at its termination, while the queue stops.
//
// THE WITNESS IS THE STOP ITSELF, found waiting in awaitKeeps among the test
// binary's goroutines: until then the case cannot tell a stop still on its way
// there from one that went past it, and a fixed wait would decide that by the
// clock.
//
// Mutation: close the connection without waiting for the keeps (awaitKeeps),
// and the stop closes it while the keep is still being settled.
func TestAStopWaitsForADeadLetterBeingKept(t *testing.T) {
	t.Parallel()
	const topic, group = "stopped.letters", "grp"
	q := openForTest(t, Config{})
	a := &attachment{q: q, key: attachKey{topic: topic, group: group},
		log: slog.New(slog.DiscardHandler)}
	ev, msg := spentDelivery(t, a)
	terming, release := make(chan struct{}), make(chan struct{})
	msg.onTerm = func() {
		close(terming)
		<-release
	}
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		a.handBack(t.Context(), msg, ev, handBackStopping)
	}()
	<-terming

	stopped := make(chan error, 1)
	go func() { stopped <- q.Stop(context.WithoutCancel(t.Context())) }()
	if !stopWaitsForKeeps(t, q) {
		close(release)
		t.Fatal("the queue's stop closed its connection while a dead letter was being kept")
	}
	if q.nc.IsClosed() {
		t.Error("the connection is closed while the stop waits for the keep")
	}
	close(release)
	<-settled
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !q.nc.IsClosed() {
		t.Error("the stop returned with its connection open")
	}
}

// stopWaitsForKeeps waits until a goroutine is inside q's awaitKeeps — a stop
// waiting for the dead letters in flight — or q's connection has closed, and
// reports which: true for the stop found waiting.
func stopWaitsForKeeps(t *testing.T, q *Queue) bool {
	t.Helper()
	bound, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	poll := time.NewTicker(time.Millisecond) //fixedwait:liveness
	defer poll.Stop()
	buf := make([]byte, 1<<20)
	for {
		if bytes.Contains(buf[:runtime.Stack(buf, true)], []byte("(*Queue).awaitKeeps")) {
			return true
		}
		if q.nc.IsClosed() {
			return false
		}
		select {
		case <-bound.Done():
			t.Fatal("the stop neither waited for the keep nor closed its connection")
		case <-poll.C:
		}
	}
}
