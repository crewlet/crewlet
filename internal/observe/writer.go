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
// design. It fires inline on the publishing node, which certainly has the
// event, so there is no broker round trip or consumer group, no way for two
// nodes to write the same row and none for a rebalance to lose one: every
// event is published once by one node, so "the publisher writes it" is the
// only exactly-once rule needed. Wired at engine construction, not beside the
// dashboard: persisting is the ENGINE's own observability (a worker-only node
// with no API still keeps its turns), and a later listener would race the first
// turn a restarted node picks up off its durable inbox.
type Writer struct{ events EventSink }

// NewWriter builds a writer, or nil for a nil sink.
//
// Nil rather than an error, so a caller registers what it gets back without a
// branch. Every node opens its store and hands its event sink in; nil is a
// caller with nowhere to write, which a test builds.
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
			log.DebugContext(ctx, "event_not_persisted", "type", ev.Type,
				"hint", "no category in observe.categories, so this type reaches "+
					"neither the event store nor the activity feed")
		}
		return
	}
	if err := w.events.Append(ctx, rec); err != nil {
		log.WarnContext(ctx, "event_write_failed", "type", rec.Type, "id", rec.ID, "error", err)
	}
}
