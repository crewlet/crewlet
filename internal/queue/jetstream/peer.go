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
	want := int(info.NumPending) + info.NumAckPending
	if want == 0 {
		return nil, nil
	}
	return q.peek(ctx, stream, topic, want)
}

// DeadLetters reports the events a subscription gave up on.
func (q *Queue) DeadLetters(ctx context.Context, topic, group string) ([]*events.Event, error) {
	subject := topics.DeadLetter(topic, group)
	stream, err := q.streamFor(ctx, subject)
	if err != nil {
		return nil, err
	}
	return q.peek(ctx, stream, subject, 256)
}

// peek reads up to limit messages from a subject WITHOUT consuming them,
// through a throwaway ack-none consumer. Inspecting a backlog must never
// change it.
func (q *Queue) peek(ctx context.Context, stream, subject string, limit int) ([]*events.Event, error) {
	cons, err := q.js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{
		FilterSubject:     subject,
		DeliverPolicy:     jetstream.DeliverAllPolicy,
		AckPolicy:         jetstream.AckNonePolicy,
		InactiveThreshold: time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("peek consumer: %w", err)
	}
	batch, err := cons.Fetch(limit, jetstream.FetchMaxWait(500*time.Millisecond))
	if err != nil {
		return nil, fmt.Errorf("peek fetch: %w", err)
	}
	var out []*events.Event
	for msg := range batch.Messages() {
		var ev events.Event
		if err := json.Unmarshal(msg.Data(), &ev); err == nil {
			out = append(out, &ev)
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
