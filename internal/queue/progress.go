package queue

import (
	"context"
	"time"
)

type progressKey struct{}

// progress is how a handler keeps the delivery it holds from timing out.
type progress struct {
	every  time.Duration
	signal func() error
}

// WithProgress states, for one handler invocation, how the handler tells the
// backend it is still working on the delivery it holds, and how often it must:
// signal resets the delivery's acknowledgement window, and every is the
// interval that keeps a waiting handler well inside it. Only a backend whose
// deliveries time out calls it; otherwise [Progress] answers false.
//
// It is for a handler waiting on something outside the queue (its seat busy
// with another turn): a timeout would spend a delivery on the wait alone (see
// [OutcomeDefer]), and signalling keeps it unspent.
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
