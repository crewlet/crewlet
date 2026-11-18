package queue

import "github.com/crewlet/crewlet/internal/events"

// Option is a setting the CONTRACT defines for a client of any backend, as
// opposed to a backend's own tuning, which each takes in its own shape. It
// exists because stamping an event's origin node must be identical on every
// broker: one stamp per backend could disagree, and the conformance suite
// could only certify each against itself.
type Option func(*Options)

// Options is what a backend resolves its [Option]s into, once, when its client
// is built.
type Options struct {
	// Node is the resolved id (config.ResolveNodeID) of the node this
	// client publishes for. Empty only in a test harness; the engine's own
	// client always names one.
	Node string
}

// WithNode names the node a client publishes for. Every event published
// through the client that names no origin of its own is stamped with it — see
// [Options.Stamp].
func WithNode(id string) Option {
	return func(o *Options) { o.Node = id }
}

// Resolve applies opts in order, the last one to set a field winning.
func Resolve(opts ...Option) Options {
	var o Options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// Stamp returns ev as this client publishes it: carrying o.Node as
// [events.Event.Node] when it names no origin of its own. Every backend calls
// it FIRST in Publish, before the event is serialized or any listener sees it.
//
// ONLY WHEN EMPTY, so the field is an ORIGIN, not a last hop: a node relaying
// another's event (a parked delivery handed back, a dead letter) must not
// rename where the work ran, which stored rows (internal/store's
// EventRecord.Node), a turn's page and an operator's logs rely on. Which store
// holds the row is a different fact (ADR-0025). It stamps a shallow COPY:
// one event published to two topics from two goroutines is legitimate, so
// writing the caller's struct would race.
func (o Options) Stamp(ev *events.Event) *events.Event {
	if ev == nil || o.Node == "" || ev.Node != "" {
		return ev
	}
	stamped := *ev
	stamped.Node = o.Node
	return &stamped
}
