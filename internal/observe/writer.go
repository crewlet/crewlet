package observe

import (
	"context"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/store"
)

var log = logging.Get("observe")

// EventSink is what this package needs from the store: somewhere to append.
type EventSink interface {
	Append(ctx context.Context, rec store.EventRecord) error
}

// Writer persists the events this node publishes.
//
// A PUBLISH LISTENER, not a subscriber, and the difference is the whole
// design. It fires inline on the node that published — the node that certainly
// has the event — so there is no broker round trip, no consumer group, and
// therefore no way for two nodes of a fleet to write the same row, and no way
// for a group's rebalance to lose one. Every event is published exactly once
// by exactly one node, which makes "the publisher writes it" the only
// exactly-once rule this needs.
//
// Wired at engine construction rather than beside the dashboard, because
// persisting what this node did is the ENGINE's own observability: a
// worker-only node with no API still keeps a record of its turns, and a
// listener registered later would race the first turn a restarted node picks
// up off its durable inbox.
type Writer struct{ events EventSink }

// NewWriter builds a writer, or nil for a node with nowhere to write.
//
// Nil rather than an error: a node with no store is a supported deployment,
// and the caller registers what it gets back without a branch.
func NewWriter(sink EventSink) *Writer {
	if sink == nil {
		return nil
	}
	return &Writer{events: sink}
}

// Listen returns the publish listener to register, or nil.
func (w *Writer) Listen() queue.PublishListener {
	if w == nil {
		return nil
	}
	return w.onPublish
}

// onPublish writes one event to the durable log.
//
// Runs in the PUBLISHING goroutine, so it must not block for long — and it
// must never fail the publish, which the listener signature already enforces
// by returning nothing. An observability write that took a turn down with it
// would trade the thing being observed for the observation.
func (w *Writer) onPublish(ctx context.Context, _ string, ev *events.Event) {
	rec, ok := Record(ev)
	if !ok {
		// Not a persisted type. Debug rather than warn: the exclusions are
		// deliberate and an unknown type is a build mismatch, and neither
		// is something a running company should shout about once per event.
		if ev != nil && Excluded(ev.Type) == "" {
			log.Debug("event_not_persisted", "type", ev.Type,
				"hint", "no category in observe.categories, so this type reaches "+
					"neither the event store nor the activity feed")
		}
		return
	}
	if err := w.events.Append(ctx, rec); err != nil {
		log.Warn("event_write_failed", "type", rec.Type, "id", rec.ID, "error", err)
	}
}
