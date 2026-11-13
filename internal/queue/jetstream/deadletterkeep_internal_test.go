package jetstream

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

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
// write failed on the cancellation, the message was terminated all the same,
// and the copy was lost with nothing wrong but the moment. The delivery that
// spent the budget is the broker's last, so nothing would have brought it
// back.
//
// Mutation: keep the copy under the loop's own context, and a stopping loop's
// hand-back keeps nothing.
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
