package queue

import "github.com/crewlet/crewlet/internal/events"

// Option is a setting the CONTRACT defines for a client of any backend, as
// opposed to a backend's own tuning — which each backend takes in its own
// shape, because only it knows what the setting means.
//
// A contract-level option exists because the one below is a promise every
// backend makes identically: an event names the node it came from, whichever
// broker carried it. Written once per backend it would be two stamps that
// could disagree about when to stamp, and the conformance suite could only
// certify each against itself.
type Option func(*Options)

// Options is what a backend resolves its [Option]s into, once, when its client
// is built.
type Options struct {
	// Node is the id of the node this client publishes for — the
	// resolved node id (config.ResolveNodeID), the same name the node's
	// presence, its leases and its broker identity carry. Empty for a
	// client built with no node, which only a test harness builds: the
	// engine's own client always names one.
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
// it FIRST in Publish — before the event is serialized, and so before any
// publish listener or consumer can see it.
//
// ONLY WHEN IT IS EMPTY, which is what makes the field an ORIGIN rather than a
// last hop. A node re-publishing an event it received — a parked delivery
// handed back, a dead letter — is relaying another node's event, and the row
// that event wrote, the work it describes and the rest of that turn's record
// are on the node that first published it. Overwriting the field on the way
// through would point every reader at a store holding none of it.
//
// A COPY, NEVER THE CALLER'S EVENT. Publish only reads what it is handed, and
// one event published to two topics from two goroutines is legitimate — so
// writing the field into the caller's struct would be a data race the race
// detector sees only in whichever test happens to do that. The copy is
// shallow: the payload, Extra and the chain are shared with the caller's,
// which is the same sharing Publish already has with every listener it hands
// the event to.
func (o Options) Stamp(ev *events.Event) *events.Event {
	if ev == nil || o.Node == "" || ev.Node != "" {
		return ev
	}
	stamped := *ev
	stamped.Node = o.Node
	return &stamped
}
