package jetstream

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// wakeEvent is the event the cases' deliveries carry, for their lines.
var wakeEvent = &events.Event{Type: "external_notification"}

// A MESSAGE'S OWN FAILURE IS MARKED ONCE, AND A NODE'S NEVER.
//
// What one attachment knows of a message's failures says whether its failure
// mark is still to be written: at the message's own failure, until this
// attachment has written it (failuremarks.go), and never at a failure of the
// node's own (queue.NakNode), which judges nothing of the message. Every
// failure counts towards the spacing, whoever's it is.
//
// Mutation: answer a mark at a node's own failure, or again once written, and
// its row is red.
func TestAMessagesOwnFailureIsMarkedOnceAndANodesNever(t *testing.T) {
	t.Parallel()
	a := &attachment{}
	if count, mark := a.failed(3, false); count != 1 || mark {
		t.Errorf("a node's own failure counted %d and asked for a mark %v, want 1 and none",
			count, mark)
	}
	if count, mark := a.failed(3, true); count != 2 || !mark {
		t.Errorf("the message's own failure counted %d and asked for a mark %v, want 2 and one",
			count, mark)
	}
	if count, mark := a.failed(3, true); count != 3 || !mark {
		t.Errorf("its failure again, the mark not yet written, counted %d and asked for a mark "+
			"%v, want 3 and one", count, mark)
	}
	a.marked(3, 101)
	if count, mark := a.failed(3, true); count != 4 || mark {
		t.Errorf("its failure once marked counted %d and asked for a mark %v, want 4 and none",
			count, mark)
	}
	if !a.failedItself(3) || a.failedItself(4) {
		t.Errorf("the record says message 3 failed as itself %v and message 4 %v, want only 3",
			a.failedItself(3), a.failedItself(4))
	}
	if mark := a.settled(3); mark != 101 {
		t.Errorf("settling a message this attachment marked at 101 answered %d: its mark is "+
			"never deleted", mark)
	}
}

// A MESSAGE THAT FAILED AS ITSELF IS MARKED ON THE BROKER, AND THE MARK GOES
// ONCE IT IS SETTLED.
//
// The member whose return spends a message's budget is rarely the one that saw
// it fail, so the failure is marked where every member reads it — the broker —
// before the message goes back (markFailed); and the member that wrote the
// mark deletes it once it settles the message, here by acknowledging it.
// Read off the broker, the leader's own answer.
//
// Mutation: write no mark at the message's own failure, and the broker holds
// none; delete nothing when the message settles, and the mark stays.
func TestAMessageThatFailedIsMarkedUntilItIsSettled(t *testing.T) {
	t.Parallel()
	const topic, group = "marked.failures", "grp"
	q := openForTest(t, Config{})
	a := &attachment{q: q, key: attachKey{topic: topic, group: group},
		log: slog.New(slog.DiscardHandler)}
	md := &jetstream.MsgMetadata{NumDelivered: 1, Stream: "CREWLET_NOTIFICATIONS",
		Sequence: jetstream.SequencePair{Stream: 41}, Timestamp: time.Now().UTC()}
	subject := failureSubject(a.key, md)

	a.apply(t.Context(), &answeredMsg{md: md}, wakeEvent, queue.Nak(errors.New("the message cannot be handled")))
	if _, marked, err := q.failureMarked(t.Context(), subject); err != nil || !marked {
		t.Fatalf("after the message's own failure the broker holds its mark %v (%v), want it held",
			marked, err)
	}

	again := *md
	again.NumDelivered++
	acked := &answeredMsg{md: &again}
	a.apply(t.Context(), acked, wakeEvent, queue.Ack())
	if acked.acked != 1 {
		t.Fatalf("the message was acknowledged %d times, want once", acked.acked)
	}
	if _, marked, err := q.failureMarked(t.Context(), subject); err != nil || marked {
		t.Errorf("once the message is settled the broker holds its mark %v (%v), want it gone",
			marked, err)
	}
}

// A NODE'S OWN FAILURE WRITES NO MARK: it judges nothing of the message, which
// is kept with its group's unhandled deliveries should a return spend its
// budget.
//
// Mutation: mark at a node's own failure, and the broker holds one.
func TestANodesOwnFailureWritesNoMark(t *testing.T) {
	t.Parallel()
	const topic, group = "unmarked.failures", "grp"
	q := openForTest(t, Config{})
	a := &attachment{q: q, key: attachKey{topic: topic, group: group},
		log: slog.New(slog.DiscardHandler)}
	md := &jetstream.MsgMetadata{NumDelivered: 1, Stream: "CREWLET_NOTIFICATIONS",
		Sequence: jetstream.SequencePair{Stream: 42}, Timestamp: time.Now().UTC()}

	a.apply(t.Context(), &answeredMsg{md: md}, wakeEvent, queue.NakNode(errors.New("the broker did not answer")))
	if _, marked, err := q.failureMarked(t.Context(), failureSubject(a.key, md)); err != nil || marked {
		t.Errorf("after the node's own failure the broker holds a mark %v (%v), want none", marked, err)
	}
}

// A MARK NAMES ONE MESSAGE OF ONE PAIR: its pair, stream, sequence and stored
// instant, so the message a stream deleted and made again stores at a reused
// sequence, or the same message delivered to another group, is never taken
// for one that failed.
//
// Mutation: leave any of the five out of the subject, and its row collides.
func TestAFailureMarkNamesOneMessageOfOnePair(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	key := attachKey{topic: "crewlet.notifications.inbound", group: "notify-inbound"}
	md := jetstream.MsgMetadata{Stream: "CREWLET_NOTIFICATIONS",
		Sequence: jetstream.SequencePair{Stream: 7}, Timestamp: at}
	base := failureSubject(key, &md)
	for name, tc := range map[string]struct {
		key attachKey
		md  func(m jetstream.MsgMetadata) jetstream.MsgMetadata
	}{
		"another topic": {attachKey{topic: "crewlet.notifications.other", group: key.group},
			func(m jetstream.MsgMetadata) jetstream.MsgMetadata { return m }},
		"another group": {attachKey{topic: key.topic, group: "another"},
			func(m jetstream.MsgMetadata) jetstream.MsgMetadata { return m }},
		"another stream": {key, func(m jetstream.MsgMetadata) jetstream.MsgMetadata {
			m.Stream = "CREWLET_AGENT"
			return m
		}},
		"another sequence": {key, func(m jetstream.MsgMetadata) jetstream.MsgMetadata {
			m.Sequence.Stream = 8
			return m
		}},
		"a reused sequence, stored again": {key, func(m jetstream.MsgMetadata) jetstream.MsgMetadata {
			m.Timestamp = at.Add(time.Nanosecond)
			return m
		}},
	} {
		other := tc.md(md)
		if got := failureSubject(tc.key, &other); got == base {
			t.Errorf("%s marks %s, the message's own subject", name, got)
		}
	}
	again := md
	again.NumDelivered = 9
	if got := failureSubject(key, &again); got != base {
		t.Errorf("a later delivery of the message marks %s, want its own %s", got, base)
	}
}
