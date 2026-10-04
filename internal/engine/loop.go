package engine

import (
	"context"
	"time"
)

// loop is a function run on a detached context until stopped, waiting for a
// run in flight.
type loop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startLoop runs run at once, and again each time the wait it answered has
// passed — the RUN answers it, so a loop paces itself on what that run found
// rather than on a second reading taken beside it. wait sleeps for a duration
// or until the context ends ([sleep]), a parameter for the tests.
func startLoop(ctx context.Context, wait func(context.Context, time.Duration),
	run func(context.Context) time.Duration) *loop {

	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l := &loop{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		for ctx.Err() == nil {
			wait(ctx, run(ctx))
		}
	}()
	return l
}

func (l *loop) stop() {
	l.cancel()
	<-l.done
}
