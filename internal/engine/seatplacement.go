package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/tracing"
)

// WHICH NODE HELD EACH SEAT WHEN: a node that takes a seat says so, with the
// tenure its lease began, and a node that lets one go says so, with why. The
// time between a release and the next tenure is time no node held the seat,
// which nothing else keeps: the Advisor's staffing reads it as stopped time,
// never as idle. A node that stops without saying so leaves a gap whose start
// nobody recorded, which its reader treats as unknown.

// seatPlaced is the event a tenure that began on node publishes.
func seatPlaced(node string, lease coord.Lease) types.SeatPlaced {
	return types.SeatPlaced{OnNode: node, Epoch: lease.Epoch, Since: lease.AcquiredAt.UTC()}
}

// seatReleased is the event a tenure that ended on node publishes.
func seatReleased(node string, lease coord.Lease, reason seat.ReleaseReason) types.SeatReleased {
	return types.SeatReleased{OnNode: node, Epoch: lease.Epoch, Reason: reason.String()}
}

// publishSeatPlacement publishes one placement event of s's, its seat fields
// filled. Best effort: a seat's claim or release never waits on, or fails
// for, its event.
func (e *Engine) publishSeatPlacement(ctx context.Context, s placement.Seat, payload events.Payload) {
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	role := e.seatRole(s.Handle)
	agentID := e.seatAgentID(role)
	if role == nil || agentID == "" {
		return
	}
	switch p := payload.(type) {
	case types.SeatPlaced:
		p.Agent, p.AgentHandle, p.RoleName = agentID, role.Handle(), role.Name
		payload = p
	case types.SeatReleased:
		p.Agent, p.AgentHandle, p.RoleName = agentID, role.Handle(), role.Name
		payload = p
	default:
		return
	}
	ev := events.NewFrom(payload, tracing.TraceOf(ctx))
	if ev == nil {
		return
	}
	ev.Source = role.Name
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "seat_placement_not_published", "seat", s.Handle, "event", ev.Type,
			"error", err.Error())
	}
}
