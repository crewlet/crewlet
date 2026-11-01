package queue

import (
	"context"
	"time"
)

// progressKey is the private context key [WithProgress]'s value travels under.
type progressKey struct{}

// progress is how a handler keeps the delivery it holds from timing out.
type progress struct {
	every  time.Duration
	signal func() error
}

// WithProgress states, for one handler invocation, how the handler tells the
// backend it is still working on the delivery it holds, and how often it must:
// signal resets the delivery's acknowledgement window, and every is the
// interval that keeps a waiting handler well inside it.
//
// ONLY A BACKEND CALLS THIS, on the delivery it is about to hand over, and
// only a backend whose deliveries time out: one with no acknowledgement window
// states nothing, and [Progress] answers false.
//
// IT EXISTS FOR A HANDLER THAT WAITS ON SOMETHING OUTSIDE THE QUEUE, the seat
// it would run a turn on being busy with another of its turns, say. Its work
// has not started, so a timeout would hand the delivery back and spend one of
// its deliveries on nothing but the wait (see [OutcomeDefer]); signalling
// keeps it, unspent, for as long as the wait lasts.
func WithProgress(ctx context.Context, every time.Duration, signal func() error) context.Context {
	if every <= 0 || signal == nil {
		return ctx
	}
	return context.WithValue(ctx, progressKey{}, progress{every: every, signal: signal})
}

// Progress reports how the handler holding ctx's delivery keeps it from timing
// out: the signal to send and how often, and false where the backend stated
// nothing because its deliveries do not time out.
func Progress(ctx context.Context) (every time.Duration, signal func() error, ok bool) {
	p, ok := ctx.Value(progressKey{}).(progress)
	if !ok {
		return 0, nil, false
	}
	return p.every, p.signal, true
}
