package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A PERSON'S MESSAGE WAITING BEHIND A BUSY SEAT IS SHOWN AS QUEUED, so a busy
// seat looks different from one that was never told. The message rides the
// priority lane, so it waits only for a running turn or an executor holding
// the seat.
//
// The routing node announces it on the seat's waiting channel once its wake
// is durable (notify.WaitingNotice); the node running a busy seat raises the
// conversation's indicator as queued ([notify.Statuses]); the turn that picks
// it up moves that indicator on in place, and the finishing dispatch takes
// the wait away ([Engine.settleWaiting]). A node that stops running the seat
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
// indicator only when this node runs the seat and the seat is busy. An idle
// seat picks it up at once, so a queued line would only flash.
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
// is already doing: a delivery dispatched on this node, or an executor holding
// it. A paused or parked seat is stopped, not busy; the Live page says so.
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
