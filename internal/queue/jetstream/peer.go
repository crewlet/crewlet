package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// Peer returns a second client on the same broker — another node of the
// fleet, in this process.
//
// The embedded server is one thing; a client of it is another. Keeping them
// separate is what lets a fleet test run two nodes against one broker, and
// it mirrors how a real fleet is arranged: each node dials the cluster, and
// nothing about one node's attachments is visible to its peers.
//
// The peer does not own the embedded server. Stopping it closes its own
// connection and leaves the broker running for whoever else is using it.
func (q *Queue) Peer(ctx context.Context) (*Queue, error) {
	// The peer never owns the broker: stopping it must leave the server
	// and every other client running, exactly as one node leaving a
	// cluster does.
	return newQueueOn(ctx, q.cfg, q.embedded, false)
}

// Backlog reports the events a subscription retains and has not acked — the
// mail an unowned seat is holding.
//
// Test-facing. A producer or consumer never asks this, but "the mail
// survived" is a property seat ownership rests on, so it must be assertable.
//
// SIZED FROM THE STREAM, NEVER FROM THE CONSUMER, and that is the whole
// subtlety of this function. The obvious form asks the durable consumer how
// much it has pending and reads that many messages — which is exact, one
// round trip, and WRONG, because a consumer's pending count is not caught up
// when the publish that produced it has already been acked. The stream stores
// the message and acks the publisher; the consumer's own accounting follows
// after. Measured on the embedded broker: immediately after an acked Publish
// the consumer reported nothing pending in about two reads in three, and in
// every one of those the stream already held the message.
//
// So the consumer-sized form answers "no mail" for mail that is demonstrably
// there. Every inspection capability this backend supplies owes
// READ-YOUR-OWN-WRITE to the conformance suite (see queuetest.Capabilities),
// and cases that assert an ABSENCE read with no wait because an absence has
// no signal to wait on — so a lagging view does not merely go quiet, it
// reports the operation under test as the thing that changed the mail. That
// is what turned an unrelated merge red on main: a baseline snapshot read
// empty, the same read a moment later saw the event, and Quiesce — which
// with nothing attached writes nothing at all — was blamed for it.
func (q *Queue) Backlog(ctx context.Context, topic, group string) ([]*events.Event, error) {
	stream, err := q.streamFor(ctx, topic)
	if err != nil {
		return nil, err
	}
	cons, err := q.js.Consumer(ctx, stream, consumerName(topic, group))
	if err != nil {
		if errors.Is(err, jetstream.ErrConsumerNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect consumer: %w", err)
	}
	info, err := cons.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("consumer info: %w", err)
	}
	stored, lastSeq, err := q.storedOn(ctx, stream, topic)
	if err != nil {
		return nil, err
	}

	// The window is (what this GROUP has finished with, what the STREAM
	// still holds]. Both bounds come from the side that can be trusted for
	// the direction it moves the answer: the stream's counts are synchronous
	// with the publish ack and cannot under-report, and the consumer's ack
	// floor can only lag BACKWARDS, which widens the window and so can never
	// hide mail. A wider window costs at most a message this group already
	// acked and another group still holds; a narrower one loses mail.
	//
	// Reading from the ack floor rather than from the head of the stream
	// also fixes what the consumer-sized form got wrong whenever two groups
	// share a subject: it asked for N messages and took the FIRST N on the
	// subject, which under interest retention are the ones the other group
	// is still holding, not this group's own.
	above := int(lastSeq) - int(info.AckFloor.Stream)
	limit := min(stored, above)
	if limit <= 0 {
		return nil, nil
	}
	return q.peek(ctx, stream, topic, info.AckFloor.Stream+1, limit)
}

// DeadLetters reports the events a subscription gave up on.
//
// Sized from the stream for the same reason as Backlog, and with the same
// second benefit: the fixed limit this used to pass meant every call waited
// out the fetch deadline, because a fetch returns early only once it has the
// whole batch it asked for and there are never 256 dead letters.
func (q *Queue) DeadLetters(ctx context.Context, topic, group string) ([]*events.Event, error) {
	subject := topics.DeadLetter(topic, group)
	stream, err := q.streamFor(ctx, subject)
	if err != nil {
		return nil, err
	}
	stored, _, err := q.storedOn(ctx, stream, subject)
	if err != nil {
		return nil, err
	}
	if stored == 0 {
		return nil, nil
	}
	return q.peek(ctx, stream, subject, 1, stored)
}

// storedOn reports how many messages the stream still holds on one subject,
// and the stream's last sequence.
//
// This is the observable that is synchronous with a publish: the server has
// stored the message before it acks the publisher, so a count taken after
// Publish returns already includes it. Nothing derived from a CONSUMER has
// that property — see Backlog.
func (q *Queue) storedOn(ctx context.Context, stream, subject string) (int, uint64, error) {
	st, err := q.js.Stream(ctx, stream)
	if err != nil {
		return 0, 0, fmt.Errorf("inspect stream %s: %w", stream, err)
	}
	// The per-subject map is populated ONLY for a requested filter, so the
	// filter is what makes the count exist rather than what narrows it.
	info, err := st.Info(ctx, jetstream.WithSubjectFilter(subject))
	if err != nil {
		return 0, 0, fmt.Errorf("stream info %s: %w", stream, err)
	}
	return int(info.State.Subjects[subject]), info.State.LastSeq, nil
}

// peekPage bounds ONE fetch, so a long list is paged rather than cut.
//
// It replaced a fixed limit of 256 passed straight to Fetch, which was a
// SILENT TRUNCATION — a subject holding more reported the first 256 and said
// nothing — and which was also what made every dead-letter read wait out the
// deadline below, since a fetch returns early only once it has the whole batch
// it asked for and there are never 256 dead letters.
const peekPage = 256

// peekWait bounds one fetch of a batch the caller has ALREADY established is
// there: both callers size their read from the stream's own count before
// asking for it. So this is a broker round trip's ceiling rather than the
// normal cost, and WAITING IS THE POINT — the messages are known to be stored,
// so the wait absorbs the server's own catch-up. It is why this stayed a timed
// fetch instead of becoming FetchNoWait, which returns whatever happens to be
// ready at that instant. That is the shape of answer the comment on Backlog
// describes, and it is what turned CI red.
const peekWait = 500 * time.Millisecond

// peekBackstop reaps a throwaway consumer whose peek never returned.
//
// A BACKSTOP, NOT A LIFETIME: every peek deletes its own consumer, so this
// only ever covers a caller that died mid-fetch, and 60x the fetch deadline is
// already generous for that. It was a minute, with nothing deleting anything —
// long enough for a polling inspection to leave thousands alive, and a leaked
// ack-none consumer is not inert: it holds INTEREST, and on an
// interest-retention stream that keeps messages the real subscription has
// already acked. An inspection that changes what the stream retains is the one
// thing this function's own contract says it must not do. Measured: thirty
// backlog reads left thirty-one consumers on the stream.
const peekBackstop = 30 * time.Second

// peekCleanup bounds the delete, which runs on a context that may already be
// cancelled.
const peekCleanup = 5 * time.Second

// peek reads up to limit messages from a subject, starting at a stream
// sequence, WITHOUT consuming them — through a throwaway ack-none consumer.
// Inspecting a backlog must never change it.
func (q *Queue) peek(
	ctx context.Context,
	stream, subject string,
	from uint64,
	limit int,
) ([]*events.Event, error) {
	cons, err := q.js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{
		FilterSubject: subject,
		// By sequence rather than DeliverAll, so a caller that knows what
		// it has already finished with can say so. A filtered consumer
		// starts at the first MATCHING message at or after the sequence,
		// so a floor pointing at another subject's message is harmless.
		DeliverPolicy:     jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:       from,
		AckPolicy:         jetstream.AckNonePolicy,
		InactiveThreshold: peekBackstop,
	})
	if err != nil {
		return nil, fmt.Errorf("peek consumer: %w", err)
	}
	defer func() {
		// WithoutCancel because a cleanup that inherits the cancellation
		// it is cleaning up after does nothing at all — and then its OWN
		// bound, because WithoutCancel drops the deadline along with the
		// cancellation.
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), peekCleanup)
		defer stop()
		_ = q.js.DeleteConsumer(cleanupCtx, stream, cons.CachedInfo().Name)
	}()

	var out []*events.Event
	for len(out) < limit {
		batch, err := cons.Fetch(min(peekPage, limit-len(out)), jetstream.FetchMaxWait(peekWait))
		if err != nil {
			return nil, fmt.Errorf("peek fetch: %w", err)
		}
		got := 0
		for msg := range batch.Messages() {
			got++
			var ev events.Event
			if err := json.Unmarshal(msg.Data(), &ev); err != nil {
				// REPORTED, never skipped. "The mail is there and this
				// build cannot read it" and "there is no mail" are
				// different facts, and these are the functions the
				// conformance suite reads its absence assertions through.
				return nil, fmt.Errorf("peek decode %s: %w", subject, err)
			}
			out = append(out, &ev)
		}
		if err := batch.Error(); err != nil {
			return nil, fmt.Errorf("peek fetch %s: %w", subject, err)
		}
		if got == 0 {
			// The stream said there was more and delivered none.
			// Stopping is the only thing that guarantees this ends.
			break
		}
	}
	return out, nil
}

// Attachments reports every (topic, group) pair THIS client is attached to.
//
// Scoped to the client, never the broker: "attached to exactly the seats I
// own" is the assertion that catches a double-consumer split-brain, and a
// fleet-wide answer cannot make it.
func (q *Queue) Attachments() [][2]string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([][2]string, 0, len(q.attachments))
	for k, atts := range q.attachments {
		if len(atts) == 0 {
			continue
		}
		out = append(out, [2]string{k.topic, k.group})
	}
	slices.SortFunc(out, func(a, b [2]string) int {
		if c := strings.Compare(a[0], b[0]); c != 0 {
			return c
		}
		return strings.Compare(a[1], b[1])
	})
	return out
}
