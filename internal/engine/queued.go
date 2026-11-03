package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A PERSON'S MESSAGE WAITING BEHIND A BUSY SEAT IS SHOWN AS QUEUED.
//
// A person's message rides its seat's priority lane, so it waits only for what
// the seat is doing when it arrives: a turn running, or an executor holding
// the seat. Until that ends nothing in the conversation said the message had
// arrived at all, and a person watching it could not tell a seat that was
// busy from one that had not been told.
//
// The routing node announces each such message on the seat's waiting channel
// once its wake is durable (notify.WaitingNotice). The node running the seat
// hears it, and when the seat is busy raises the conversation's indicator in
// its queued state, saying how many tasks are ahead of it ([notify.Statuses]).
// The turn that picks the message up joins the same indicator and moves it on
// to its phase in place, and the dispatch that finishes with the message takes
// its wait away ([Engine.settleWaiting]). A node that stops running the seat
// takes every indicator of it down, queued ones included.

// startWaitingWatch listens on every seat's waiting channel. Best effort: a
// node that cannot listen shows no queued lines, which costs nobody a message.
func (e *Engine) startWaitingWatch(ctx context.Context) {
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	watchCtx := context.WithoutCancel(ctx)
	unsub, err := e.backends.Queue.Listen(watchCtx, topics.SeatWaitingAll,
		func(subject string, payload []byte) { e.heardWaiting(watchCtx, subject, payload) })
	if err != nil {
		log.WarnContext(ctx, "waiting_watch_not_started", "error", err.Error(),
			"detail", "a person's message waiting behind a busy seat is not shown as queued "+
				"on this node; the message itself is delivered as usual")
		return
	}
	e.waitingWatch = unsub
}

// stopWaitingWatch stops listening. The queued indicators themselves come
// down with the status drivers.
func (e *Engine) stopWaitingWatch(ctx context.Context) {
	if e.waitingWatch == nil {
		return
	}
	if err := e.waitingWatch(ctx); err != nil {
		log.DebugContext(ctx, "waiting_watch_not_stopped", "error", err.Error())
	}
	e.waitingWatch = nil
}

// heardWaiting is one announcement: queue the person's message on the seat's
// indicator when this node runs the seat and the seat is busy, and do nothing
// otherwise. An idle seat picks the message up at once, and a line saying it
// is queued would only flash before the seat's own working line.
func (e *Engine) heardWaiting(ctx context.Context, subject string, payload []byte) {
	seat, ok := topics.SeatFromWaiting(subject)
	if !ok {
		return
	}
	handle, ok := e.seatHandle(seat.String())
	if !ok {
		return
	}
	if _, owned := e.node.Host().MayStart(handle); !owned || !e.seatBusy(handle) {
		return
	}
	notice, err := notify.DecodeWaitingNotice(payload)
	if err != nil || notice.Wake == "" {
		log.DebugContext(ctx, "waiting_notice_unreadable", "seat", handle)
		return
	}
	e.Status().Queue(ctx, handle, notice.Wake, notice.Metadata)
}

// seatBusy reports whether a message reaching the seat now waits for what it
// is already doing: a delivery of it being dispatched on this node, or one of
// its executors holding it. A seat that is paused or parked is not busy but
// stopped, which its own state on the Live page says.
func (e *Engine) seatBusy(handle string) bool {
	d := e.dispatch
	if d == nil {
		return false
	}
	if d.Running(handle) {
		return true
	}
	return d.Conditions != nil && d.Conditions(handle).SeatHeldBySandbox
}

// settleWaiting takes away the queued waits of messages a dispatch has
// finished with, on every chat backend.
func (e *Engine) settleWaiting(ctx context.Context, handle string, wakes []string) {
	e.Status().Dequeue(ctx, handle, wakes)
}
