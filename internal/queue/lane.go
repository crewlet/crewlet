package queue

import (
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// PRIORITY LANES: a person waiting on a seat is never queued behind the seat's
// background work.
//
// # The failure this exists to end
//
// A seat's inbox was one first-in, first-out mailbox, and a drain reads only
// its head ([BatchOptions.EffectiveMaxBatch] messages). A seat parked on a
// provider limit for hours came back to a backlog of tracker notices, and a
// person's direct question that arrived during the park sat behind every one
// of them: the seat ran one turn per notice, oldest first, and the person was
// still waiting hours after the limit cleared. No order a drain chooses can
// fix that, because the person's message is not in the drain at all until the
// backlog in front of it has been consumed.
//
// # The mechanism
//
// A topic that carries a lane ([HasLane]) is two mailboxes under one
// subscription. An event marked [Prioritize] and published to the topic is
// written to its lane ([LaneOf]); every other event to the topic itself. Every
// subscription on the topic consumes the lane FIRST: a drain takes what the
// lane holds before it reads the topic, and [OrderForDispatch] dispatches a
// partition holding a priority event ahead of every other partition of the
// drain. Within the lane, and within the topic, the order is what it always
// was.
//
// ONE SUBSCRIPTION, NOT TWO, and that is the property every caller relies on.
// EnsureSubscription, DeleteSubscription, Quiesce, Unquiesce, PauseTopic,
// ResumeTopic and Detach name the topic and act on both mailboxes, so a seat
// that is paused, parked, held by one of its executors or handed to another
// node holds its lane exactly as it holds the rest of its mail. A lane that
// could be consumed while its topic was held would run a turn on a seat
// somebody stopped.
//
// THE MARK RIDES THE EVENT, never the call. A delivery handed back, parked or
// requeued is published again as the event it is, so it returns to the lane it
// left; a publisher that had to choose the subject itself would put every
// requeued priority event back behind the backlog it was meant to skip.

// PriorityField is the envelope bag key that marks an event for its topic's
// priority lane. See [Prioritize].
const PriorityField = "priority_lane"

// LaneSuffix is appended to a laned topic to name its priority lane.
const LaneSuffix = ".priority"

// Prioritize marks ev to travel its topic's priority lane.
//
// The mark is on the envelope's bag, which every backend carries verbatim and
// every republish keeps. A nil event is left alone. Publish refuses a nil
// event, and refusing it here as well would make the mark a second place that
// decides that.
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

// HasLane reports whether a topic carries a priority lane.
//
// A SEAT'S INBOX, and only a seat's inbox: it is the one mailbox a person's
// message and a seat's background work share, so it is the one place a person
// can be queued behind work nobody is waiting for. Every other topic is a
// single mailbox, and a mark on an event published to one is ignored. Each
// lane costs the broker a second durable consumer, which is a price worth
// paying for a seat and for nothing else.
func HasLane(topic string) bool {
	_, ok := topics.SeatFromInbox(topic)
	return ok
}

// LaneOf names a laned topic's priority lane.
//
// The lane sits under the topic's own subject, so it lives on whatever stream
// carries the topic, and it is not itself a topic anything subscribes to:
// [topics.SeatFromInbox] does not read it as an inbox, so it carries no lane
// of its own.
func LaneOf(topic string) string { return topic + LaneSuffix }

// SubjectFor is the subject a publish of ev to topic is written to: the
// topic's lane for a marked event on a laned topic, and the topic itself for
// everything else. The one rule every backend's Publish applies.
func SubjectFor(topic string, ev *events.Event) string {
	if Prioritized(ev) && HasLane(topic) {
		return LaneOf(topic)
	}
	return topic
}
