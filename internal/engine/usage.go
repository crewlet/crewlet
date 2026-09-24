package engine

import (
	"context"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/usage"
)

// The usage domain's writer, armed — the loop that turns this node's own event
// log into the replicated record of what its seats and schedules did each day.
//
// # Every node runs one, and no node runs anybody else's
//
// Unlike the embedding duty beside it this is not a fleet singleton: a node's
// event log holds only what ITS seats did, so the node is the only thing that
// can derive its own day, and the node in every subject it writes is what
// keeps N writers from ever meeting on one. Without it the domain is
// registered, applied and replicated over an empty stream — and every spend
// answer is exactly as partial as it was before the domain existed, while
// every surface reports a healthy log.

// usageLoop is one node's usage publisher and the goroutine running it.
type usageLoop struct {
	stop context.CancelFunc
	done chan struct{}
}

// startUsage arms this node's usage publisher, or does nothing on a node that
// runs no state log.
func (e *Engine) startUsage(ctx context.Context, s *stateLog) {
	if s == nil || e.backends == nil || e.backends.Store == nil {
		return
	}
	running := s.domains[usage.Domain{}.Name()]
	if running == nil || running.publisher == nil {
		return
	}
	publisher, err := usage.NewPublisher(usage.PublisherDeps{
		Store:  e.backends.Store,
		Log:    running.publisher,
		NodeID: s.nodeID,
		// THE CLOCK AND THE CHART ARE READ PER TICK, from whichever epoch
		// is current: an apply that moves the company's timezone moves
		// the next day's cut with it, and a seat renamed today is named
		// by its new handle from the next flush.
		Zone:   e.Zone,
		Handle: e.seatHandle,
		Logger: log,
	})
	if err != nil {
		log.WarnContext(ctx, "usage_publisher_unbuilt", "err", err,
			"detail", "this node's spend, turns and reads do not replicate, "+
				"so fleet-wide history omits this node until it restarts")
		return
	}
	// DETACHED from the caller's context, for the embedding duty's reason:
	// a loop bound to a signal context stops at SIGTERM, a moment before
	// the appliers it publishes into.
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	u := &usageLoop{stop: stop, done: make(chan struct{})}
	e.usage = u
	go func() {
		defer close(u.done)
		publisher.Run(loop)
	}()
}

// stopUsage ends the loop, waiting for an in-flight flush.
func (e *Engine) stopUsage() {
	if e.usage == nil {
		return
	}
	e.usage.stop()
	<-e.usage.done
	e.usage = nil
}

// seatHandle names a seat by its derived id from the current chart, and false
// for an id the chart no longer holds.
func (e *Engine) seatHandle(agentID string) (string, bool) {
	c := e.Company()
	if c == nil || c.Org == nil {
		return "", false
	}
	id, err := uuid.Parse(agentID)
	if err != nil {
		return "", false
	}
	seat := c.Org.AgentSeatByID(id)
	if seat == nil {
		return "", false
	}
	return seat.Handle(), true
}
