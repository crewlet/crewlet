package observe

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// Ingester is what a live projection needs from this package. Satisfied by
// api/stream.Service.
//
// An interface rather than the concrete service so that this package — which
// the engine depends on for the writer above — does not drag the HTTP layer in
// behind it.
type Ingester interface {
	Ingest(livestate.Envelope)
}

// Projector feeds this node's live projection from the whole company's events.
//
// AN EPHEMERAL BROADCAST SUBSCRIPTION, and it has to be: a dashboard served by
// node B must show turns that ran on node A. A competing-consumer group would
// hand each event to exactly one node's projection, so which turns a browser
// saw would depend on which node answered its socket — and a refresh would
// tell a different story than the page it replaced.
//
// It is also why this cannot be a publish listener like [Writer]: a listener
// only ever sees what its OWN node published, which is the same defect from
// the other side.
type Projector struct {
	live  Ingester
	queue queue.EventQueue
	stop  queue.Unsubscribe
}

// NewProjector builds a projector, or nil for a node that serves no dashboard.
func NewProjector(q queue.EventQueue, live Ingester) *Projector {
	if q == nil || live == nil {
		return nil
	}
	return &Projector{queue: q, live: live}
}

// Start attaches the broadcast subscription.
func (p *Projector) Start(ctx context.Context) error {
	if p == nil {
		return nil
	}
	stop, err := p.queue.SubscribeStream(ctx, topics.EventsWildcard, p.onEvent)
	if err != nil {
		return fmt.Errorf("observe: subscribe %s: %w", topics.EventsWildcard, err)
	}
	p.stop = stop
	log.InfoContext(ctx, "projector_started", "pattern", topics.EventsWildcard)
	return nil
}

// Stop releases the subscription.
//
// Released rather than left to the process, because it is EPHEMERAL only by
// contract: a backend that materialises one as a durable subscription needs to
// be told this one is finished, or a node that comes and goes leaves a trail
// of them accruing mail nobody reads.
func (p *Projector) Stop(ctx context.Context) {
	if p == nil || p.stop == nil {
		return
	}
	if err := p.stop(ctx); err != nil {
		log.WarnContext(ctx, "projector_unsubscribe_failed", "error", err)
	}
	p.stop = nil
}

// onEvent feeds one event to the projection.
func (p *Projector) onEvent(_ context.Context, _ string, ev *events.Event) {
	if env, ok := Envelope(ev); ok {
		p.live.Ingest(env)
	}
}
