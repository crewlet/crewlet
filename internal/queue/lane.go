package queue

import (
	"maps"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// PRIORITY LANES: a person waiting on a seat is never queued behind the seat's
// background work, which a FIFO inbox drained from its head cannot promise. A
// laned topic ([HasLane]) is two mailboxes under ONE subscription: a
// [Prioritize]d event is written to the lane ([SubjectFor]), every drain takes
// the lane first, and [OrderForDispatch] dispatches its partition first. Every
// call naming the topic (EnsureSubscription, Quiesce, PauseTopic, Detach, ...)
// acts on both, so a paused, parked or handed-off seat holds its lane too. The
// mark rides the event, never the call, so a requeue returns to its lane.

// A drain fills from the topic's head up to [BatchOptions.EffectiveMaxBatch]
// after the lane: a same-partition topic event in that window joins the
// person's turn, one deeper is delivered later as its own partition (a chat
// message already answered is dropped as worked by the completion ledger's
// chat key). Reaching past the window would take mail outside the consumer's
// ack state, which no ack, Nak or crash could keep exact.

// PriorityField is the envelope bag key that marks an event for its topic's
// priority lane. See [Prioritize].
const PriorityField = "priority_lane"

// Prioritize marks ev to travel its topic's priority lane, on the envelope's
// bag, which every backend and republish keeps verbatim. A nil event is left
// alone: Publish is the one place that refuses it.
func Prioritize(ev *events.Event) {
	if ev == nil {
		return
	}
	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	ev.Payload[PriorityField] = true
}

// Prioritized reports whether ev is marked for its topic's priority lane.
func Prioritized(ev *events.Event) bool {
	if ev == nil {
		return false
	}
	marked, _ := ev.Payload[PriorityField].(bool)
	return marked
}

// HasLane reports whether a topic carries a priority lane: only a seat's
// inbox, the one mailbox a person's message shares with background work. A
// lane costs a second durable consumer, so every other topic is a single
// mailbox and ignores the mark.
func HasLane(topic string) bool {
	_, ok := topics.SeatFromInbox(topic)
	return ok
}

// LaneOf names a laned topic's priority lane, under the topic's own subject so
// it lives on the topic's stream. It is no inbox to [topics.SeatFromInbox], so
// it carries no lane of its own.
func LaneOf(topic string) string { return topics.AgentInboxLane(topic) }

// SubjectFor is the subject a publish of ev to topic is written to: the
// topic's lane for a marked event on a laned topic, and the topic itself for
// everything else. The one rule every backend's Publish applies.
func SubjectFor(topic string, ev *events.Event) string {
	if Prioritized(ev) && HasLane(topic) {
		return LaneOf(topic)
	}
	return topic
}

// Together is a partition as a requeue publishes it: when any of its events is
// marked for the priority lane, every one of them is, so the partition returns
// to the lane as one unit rather than its siblings to the topic's tail. The
// events it marks are COPIES: the caller's are the delivery it still answers
// for, and a requeue must not change what that delivery held.
func Together(evs []*events.Event) []*events.Event {
	marked := false
	for _, ev := range evs {
		if Prioritized(ev) {
			marked = true
			break
		}
	}
	if !marked {
		return evs
	}
	out := make([]*events.Event, len(evs))
	for i, ev := range evs {
		if ev == nil || Prioritized(ev) {
			out[i] = ev
			continue
		}
		cp := *ev
		cp.Payload = maps.Clone(ev.Payload)
		Prioritize(&cp)
		out[i] = &cp
	}
	return out
}

// Waiting is the mail a subscription holds that no handler is running on yet
// ([EventQueue.Waiting]), per mailbox: Priority on a laned topic's priority
// lane, Background on the topic itself. A topic with no lane counts
// everything as Background.
type Waiting struct {
	Priority   int
	Background int
}

// Total is everything waiting, in both mailboxes.
func (w Waiting) Total() int { return w.Priority + w.Background }
