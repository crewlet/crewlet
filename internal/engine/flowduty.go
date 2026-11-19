package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/worklet"
)

// The flow duty records each change of an item's flow and tells a person when
// nobody is moving it (ADR-0162; worklet.FlowDuty). A fleet singleton every
// minute, held where the live projection is seeded ([Engine.liveDuty]) and the
// tracker's estate is held: a node with no estate has no items to read, and
// one with no live posture would read every stopped seat's work as moving.

// flowDutyName is the duty's lease. Its TTL is three ticks, as the delegated
// sweep's is: a holder that died is replaced within three minutes, and one that
// lives renews it every tick.
const (
	flowDutyName = "tracker-flow"
	flowDutyTTL  = 3 * worklet.FlowDutyInterval
)

// flowDutyLoop is the duty's loop on this node.
type flowDutyLoop struct {
	engine *Engine
	lease  liveLease
	now    func() time.Time
	stop   context.CancelFunc
	done   chan struct{}

	// duty is built once this node's tracker exists, and kept: it holds the
	// position of its own last write.
	duty *worklet.FlowDuty
}

// startFlowDuty arms the duty on every node; each tick asks whether this node
// may hold it.
func (e *Engine) startFlowDuty(ctx context.Context) {
	if e.backends == nil {
		return
	}
	l := &flowDutyLoop{engine: e, lease: e.liveDuty(flowDutyName, flowDutyTTL),
		now: time.Now, done: make(chan struct{})}
	// DETACHED from the caller's context, as every long-running loop here is.
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	l.stop = stop
	e.flowDuty = l
	go l.run(loop)
}

// stopFlowDuty ends the duty, waiting for a tick in flight.
func (e *Engine) stopFlowDuty() {
	if e.flowDuty == nil {
		return
	}
	e.flowDuty.stop()
	<-e.flowDuty.done
	e.flowDuty = nil
}

func (l *flowDutyLoop) run(ctx context.Context) {
	defer close(l.done)
	ticker := time.NewTicker(worklet.FlowDutyInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		l.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pass is one tick: hold the duty where this node may, then read and tell.
func (l *flowDutyLoop) pass(ctx context.Context) {
	duty := l.dutyHere()
	was := l.lease.held
	held, err := l.lease.holds(ctx, duty != nil && duty.Live())
	switch {
	case err != nil:
		log.WarnContext(ctx, "flow_duty_unclaimed", "error", err.Error())
		return
	case held && !was:
		log.InfoContext(ctx, "flow_duty_held", "detail", "this node serves the live "+
			"projection and holds the tracker, so it records and tells items' flow")
	case !held && was:
		log.InfoContext(ctx, "flow_duty_given_back", "detail", "this node no longer "+
			"reads a live posture, so a node that does takes the duty")
	}
	if !held {
		return
	}
	if _, err := duty.Tick(ctx, l.now()); err != nil {
		log.WarnContext(ctx, "flow_duty_tick_failed", "error", err.Error())
	}
}

// dutyHere is the duty over this node's tracker, nil while it holds none.
func (l *flowDutyLoop) dutyHere() *worklet.FlowDuty {
	if l.duty != nil {
		return l.duty
	}
	n, core := l.engine.native.Load(), l.engine.core.Load()
	if n == nil || n.writer == nil || n.trackerReader == nil || core == nil {
		return nil
	}
	duty, err := worklet.NewFlowDuty(worklet.FlowDutyDeps{
		Reader: n.trackerReader, Writer: n.writer,
		Chart: flowChart{liveLeads: liveLeads{engine: l.engine}}, NodeID: core.nodeID,
	})
	if err != nil {
		return nil
	}
	l.duty = duty
	return duty
}

// flowChart is the flow duty's read of the current chart: its leads, and
// which seats are people to tell.
type flowChart struct{ liveLeads }

// Person reports whether handle is a person's seat in the current chart.
func (c flowChart) Person(handle string) bool {
	company := c.engine.Company()
	if company == nil || company.Org == nil {
		return false
	}
	seat := company.Org.SeatByHandle(handle)
	return seat != nil && seat.IsHuman()
}
