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
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// Peer returns a second client on the same broker: another node of the fleet,
// in this process, as a fleet test needs. The embedded server and its clients
// are separate things, as in a real fleet where each node dials the cluster.
//
// The peer does not own the broker: stopping it leaves the server running for
// every other client. Nor does it inherit this client's node, which would name
// the wrong origin on everything it sent; it names itself through opts.
func (q *Queue) Peer(ctx context.Context, opts ...queue.Option) (*Queue, error) {
	return newQueueOn(ctx, q.cfg, q.embedded, false, queue.Resolve(opts...))
}

// Backlog reports the events a subscription retains and has not acked: the
// mail an unowned seat is holding. Test-facing, because seat ownership rests
// on "the mail survived".
//
// SIZED FROM THE STREAM, NEVER FROM THE CONSUMER: the stream stores and acks a
// publish before the consumer's pending count catches up, so a consumer-sized
// read answers "no mail" for mail that is there. The conformance suite owes
// READ-YOUR-OWN-WRITE (queuetest.Capabilities) and asserts absences with no
// wait, so a lagging view would blame the operation under test for the mail.
//
// On a laned topic the lane's mail is part of it ([queue.HasLane]), listed
// first, as the mailbox the subscription takes first.
func (q *Queue) Backlog(ctx context.Context, topic, group string) ([]*events.Event, error) {
	stream, err := q.streamFor(ctx, topic)
	if err != nil {
		return nil, err
	}
	var out []*events.Event
	if queue.HasLane(topic) {
		lane := queue.LaneOf(topic)
		held, laneErr := q.backlogOf(ctx, stream, lane, consumerName(lane, group))
		if laneErr != nil {
			return nil, laneErr
		}
		out = append(out, held...)
	}
	held, err := q.backlogOf(ctx, stream, topic, consumerName(topic, group))
	if err != nil {
		return nil, err
	}
	return append(out, held...), nil
}

// Waiting reports what a subscription holds that no handler of this client is
// running on ([queue.EventQueue.Waiting]).
//
// From each mailbox consumer's own state: undelivered plus delivered and
// unacknowledged, less what this client's handlers are running on
// ([attachment.running]). One fetched and not yet dispatched, or handed back
// for redelivery, stays counted. Consumer state trails a publish by a moment,
// which a display can carry and a decision could not.
func (q *Queue) Waiting(ctx context.Context, topic, group string) (queue.Waiting, error) {
	stream, err := q.streamFor(ctx, topic)
	if err != nil {
		return queue.Waiting{}, err
	}
	background, err := q.unfinished(ctx, stream, consumerName(topic, group))
	if err != nil {
		return queue.Waiting{}, err
	}
	var priority int
	if queue.HasLane(topic) {
		lane := queue.LaneOf(topic)
		if priority, err = q.unfinished(ctx, stream, consumerName(lane, group)); err != nil {
			return queue.Waiting{}, err
		}
	}
	runPriority, runBackground := q.runningOn(attachKey{topic, group})
	return queue.Waiting{
		Priority:   max(priority-runPriority, 0),
		Background: max(background-runBackground, 0),
	}, nil
}

// unfinished is what one durable consumer has not finished: undelivered plus
// delivered and unacknowledged. A consumer the broker does not hold has
// nothing.
func (q *Queue) unfinished(ctx context.Context, stream, name string) (int, error) {
	cons, err := q.js.Consumer(ctx, stream, name)
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspect consumer %s: %w", name, q.brokerFailed(ctx, err))
	}
	info, err := cons.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("consumer info %s: %w", name, q.brokerFailed(ctx, err))
	}
	return int(info.NumPending) + info.NumAckPending, nil
}

// runningOn is the mail this client's handlers on one subscription are running
// on, per mailbox, summed over its attachments.
func (q *Queue) runningOn(key attachKey) (priority, background int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, a := range q.attachments[key] {
		priority += int(a.running.priority.Load())
		background += int(a.running.background.Load())
	}
	return priority, background
}

// backlogOf is [Queue.Backlog] for one durable consumer reading one subject.
func (q *Queue) backlogOf(ctx context.Context, stream, topic, name string) ([]*events.Event, error) {
	cons, err := q.js.Consumer(ctx, stream, name)
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
	// still holds]. Each bound can only err wide: the stream's counts are
	// synchronous with the publish ack, and the ack floor lags backwards. A
	// wider window costs a message this group acked that another still
	// holds; a narrower one loses mail. From the ack floor, not the head: under interest retention
	// the first N on a shared subject are another group's.
	above := int(lastSeq) - int(info.AckFloor.Stream)
	limit := min(stored, above)
	if limit <= 0 {
		return nil, nil
	}
	return q.peek(ctx, stream, []string{topic}, info.AckFloor.Stream+1, limit)
}

// DeadLetters reports the events a subscription gave up on: what both of its
// pair's dead-letter subjects hold, its poison and its unhandled deliveries
// ([queue.RetiredTo]), in the order the stream kept them.
//
// Sized from the stream, as Backlog is, which also lets the fetch return at
// once: a fetch returns early only once it has the whole batch it asked for.
func (q *Queue) DeadLetters(ctx context.Context, topic, group string) ([]*events.Event, error) {
	subjects := []string{topics.DeadLetter(topic, group), topics.Unhandled(topic, group)}
	stream, err := q.streamFor(ctx, subjects[0])
	if err != nil {
		return nil, err
	}
	stored := 0
	for _, subject := range subjects {
		held, _, err := q.storedOn(ctx, stream, subject)
		if err != nil {
			return nil, err
		}
		stored += held
	}
	if stored == 0 {
		return nil, nil
	}
	return q.peek(ctx, stream, subjects, 1, stored)
}

// storedOn reports how many messages the stream still holds on one subject,
// and the stream's last sequence: synchronous with a publish, since the server
// stores before it acks, which nothing derived from a consumer is (Backlog).
func (q *Queue) storedOn(ctx context.Context, stream, subject string) (int, uint64, error) {
	st, err := q.js.Stream(ctx, stream)
	if err != nil {
		return 0, 0, fmt.Errorf("inspect stream %s: %w", stream, err)
	}
	// The per-subject map is populated ONLY for a requested filter, so the
	// filter is what makes the count exist.
	info, err := st.Info(ctx, jetstream.WithSubjectFilter(subject))
	if err != nil {
		return 0, 0, fmt.Errorf("stream info %s: %w", stream, err)
	}
	return int(info.State.Subjects[subject]), info.State.LastSeq, nil
}

// peekPage bounds ONE fetch, so a long list is paged rather than silently cut.
const peekPage = 256

// peekWait bounds one fetch of a batch the caller has already established is
// stored, so it is a round trip's ceiling, not the normal cost. WAITING IS THE
// POINT: it absorbs the server's catch-up, where FetchNoWait returns whatever
// is ready that instant, the lagging answer Backlog describes.
const peekWait = 500 * time.Millisecond

// peekBackstop reaps a throwaway consumer whose peek never returned.
//
// A BACKSTOP, NOT A LIFETIME: every peek deletes its own consumer, so this
// covers only a caller that died mid-fetch, for which 60x peekWait is
// generous. Kept short because a leaked
// ack-none consumer holds INTEREST, and on an interest-retention stream keeps
// messages the real subscription acked: an inspection that changes what the
// stream retains.
const peekBackstop = 30 * time.Second

// peekCleanup bounds the delete, which runs on a context that may already be
// cancelled.
const peekCleanup = 5 * time.Second

// peek reads up to limit messages from subjects, in the stream's order,
// starting at a stream sequence, WITHOUT consuming them — through a throwaway
// ack-none consumer. Inspecting a backlog must never change it.
func (q *Queue) peek(
	ctx context.Context,
	stream string,
	subjects []string,
	from uint64,
	limit int,
) ([]*events.Event, error) {
	subject := strings.Join(subjects, ", ")
	cons, drop, err := q.throwaway(ctx, stream, jetstream.ConsumerConfig{
		FilterSubjects: subjects,
		// By sequence, so a caller can skip what it has finished with. A
		// filtered consumer starts at the first MATCHING message at or after
		// it, so a floor on another subject's message is harmless.
		DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:   from,
	})
	if err != nil {
		return nil, fmt.Errorf("peek consumer: %w", err)
	}
	defer drop()

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
				// REPORTED, never skipped: "this build cannot read the mail"
				// and "there is no mail" are different facts, and the
				// conformance suite's absence assertions read through here.
				return nil, fmt.Errorf("peek decode %s: %w", subject, err)
			}
			out = append(out, &ev)
		}
		if err := batch.Error(); err != nil {
			return nil, fmt.Errorf("peek fetch %s: %w", subject, err)
		}
		if got == 0 {
			// A SHORT READ IS THE ORDINARY END OF THE WINDOW, never an error,
			// and the only thing that ends this loop. limit is an upper bound:
			// the per-subject total includes mail below this group's ack floor
			// that a sibling group still holds, and the stream's last sequence
			// spans every subject. Erroring here would fail every inspection of
			// a shared subject; unlike a lagging read, this one has asked the
			// broker and waited peekWait.
			break
		}
	}
	return out, nil
}

// Attachments reports every (topic, group) pair THIS client is attached to.
//
// Scoped to the client, never the broker: "attached to exactly the seats I
// own" is the assertion that catches a double-consumer split-brain.
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
