package pulsar

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// This file is the OBSERVABILITY surface: what a subscription is holding,
// what it gave up on, which seats this node is serving, and whether it has
// stopped taking work. None of it is in the EventQueue contract, because a
// producer or a consumer never asks — but every one of these is a property
// seat ownership rests on, so each must be assertable from outside. The
// conformance suite reads them through Capabilities.

// peekTimeout bounds a whole backlog inspection. Separate from adminTimeout,
// which bounds ONE call: a deep backlog is many calls, and a caller must not
// hang on a broker that has gone quiet mid-walk.
const peekTimeout = 30 * time.Second

// readTimeout bounds reading a dead-letter topic to its end.
const readTimeout = 5 * time.Second

// Peer returns a second client on the same broker — another node of the
// fleet, in this process.
//
// The broker is one thing; a client of it is another. Keeping them separate
// is what lets a fleet test run two nodes against one estate, and it mirrors
// how a real fleet is arranged: each node dials the cluster, and nothing
// about one node's attachments is visible to its peers.
func (q *Queue) Peer(ctx context.Context) (*Queue, error) {
	return Open(ctx, q.cfg)
}

// Backlog reports the events a subscription retains and has NOT DELIVERED —
// the mail an unowned seat is holding, which is what a successor receives.
//
// Not everything unacked: a message a consumer is currently holding is work
// in progress, and it joins the backlog when that consumer hands it back —
// on Pulsar, when it closes. The distinction is the difference between "the
// mailbox is filling up" and "the seat is busy", and they are opposite facts.
//
// Read by PEEKING through the admin API rather than by consuming: a
// throwaway consumer would join the Shared subscription it is inspecting and
// take a share of that seat's live traffic, which is the same hazard
// EnsureSubscription exists to avoid. Inspecting a mailbox must not change it.
func (q *Queue) Backlog(ctx context.Context, topic, group string) ([]*events.Event, error) {
	ctx, cancel := context.WithTimeout(ctx, peekTimeout)
	defer cancel()
	payloads, err := q.admin.PeekBacklog(ctx, topic, group)
	if err != nil {
		return nil, err
	}
	return decodeAll(payloads), nil
}

// DeadLetters reports the events a subscription gave up on.
//
// Read with a READER, not a consumer: a reader takes no share of any
// subscription's traffic and moves no cursor, so an operator inspecting
// poison cannot accidentally consume it. The dead-letter topic keeps its
// backlog because the DLQ policy creates a retaining subscription on it —
// see dlqRetainSubscription.
func (q *Queue) DeadLetters(ctx context.Context, topic, group string) ([]*events.Event, error) {
	full := q.cfg.fullTopic(topics.DeadLetter(topic, group))
	reader, err := q.client.CreateReader(pulsar.ReaderOptions{
		Topic:                   full,
		StartMessageID:          pulsar.EarliestMessageID(),
		StartMessageIDInclusive: true,
	})
	if err != nil {
		// A dead-letter topic that does not exist is the normal state of a
		// healthy subscription, not a failure to report.
		if strings.Contains(err.Error(), "TopicNotFound") {
			return nil, nil
		}
		return nil, fmt.Errorf("open dead-letter reader for %s/%s: %w", topic, group, err)
	}
	defer reader.Close()

	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var out []*events.Event
	for reader.HasNext() {
		msg, err := reader.Next(ctx)
		if err != nil {
			return out, fmt.Errorf("read dead letters for %s/%s: %w", topic, group, err)
		}
		var ev events.Event
		if err := json.Unmarshal(msg.Payload(), &ev); err == nil {
			out = append(out, &ev)
		}
	}
	return out, nil
}

func decodeAll(payloads [][]byte) []*events.Event {
	out := make([]*events.Event, 0, len(payloads))
	for _, raw := range payloads {
		var ev events.Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}
		out = append(out, &ev)
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

// Quiescing reports whether this process has stopped taking work on a
// subscription.
//
// Distinct from a pause hold and separately observable because the two are
// cleared by different things — a hold by the subsystem that took it, a
// quiesce by Unquiesce or by detaching — and because a stale quiesce is
// invisible from outside until someone attaches, which is what lets one sit
// unnoticed long enough to strand a seat.
func (q *Queue) Quiescing(topic, group string) bool {
	for _, a := range q.lookup(topic, group) {
		if a.quiesced.Load() {
			return true
		}
	}
	return false
}

var _ interface {
	queue.EventQueue
	Attachments() [][2]string
	PauseHolds(topic, group string) []string
	Quiescing(topic, group string) bool
} = (*Queue)(nil)
