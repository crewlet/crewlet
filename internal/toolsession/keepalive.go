package toolsession

import (
	"context"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A RUNNING CALL'S ANSWER, KEPT GOING ([Server.KeepAnswersGoing]).
//
// Over streamable HTTP the SDK writes nothing on a call's response until its
// first event, so a tool that takes minutes leaves a harness's timer a silent
// one. A progress on the call's token starts the response as the call starts
// running; another every cadence reads as a call still at work. No total, no
// message (a harness may show it), under the request's context so it lands on
// the call's own response.
// IT STOPS, never writing again, when the dispatcher returned (the handler
// waits for the beat first, so no progress races the answer), the request's
// context ended, or a write failed. A FAILED WRITE IS THE RESPONSE GONE: the
// SDK keeps nothing to resume, so [Call.Abandoned] reports it at the next beat.

// beat keeps one call's answer going while its tool runs.
type beat struct {
	// ctx is the request's own context, which routes a notification onto the
	// call's own response (the SDK reads the request's id off it): held,
	// against the rule that a context is threaded rather than stored,
	// because the beat writes on the request's behalf from a goroutine of
	// its own for as long as the call runs, and the request's context has no
	// other way to reach it.
	ctx context.Context
	// session is the MCP session the request came on, and token the progress
	// token the harness gave the call: nil where it gave none, and then
	// nothing is written.
	session *mcp.ServerSession
	token   any
	every   time.Duration

	// unread is the request's context, ended too by lose once a progress
	// could not be written: what [Call.Abandoned] is handed of it. A child
	// of the request's, so a cancelled request closes it as it is cancelled
	// rather than a goroutine later.
	unread context.Context
	lose   context.CancelFunc

	// once starts the beat at most once, and is spent by [beat.stop] where
	// the dispatcher never started it, so a start after the answer starts
	// nothing. quit asks a started beat to stop, and done is closed once it
	// has: nil while it has not started, and set inside once.
	once sync.Once
	quit chan struct{}
	done chan struct{}
}

// newBeat is the beat of one call, not yet started: ctx and session are the
// request's, token its progress token.
func newBeat(ctx context.Context, session *mcp.ServerSession, token any, every time.Duration) *beat {
	unread, lose := context.WithCancel(ctx)
	return &beat{ctx: ctx, session: session, token: token, every: every,
		unread: unread, lose: lose, quit: make(chan struct{})}
}

// start is [Call.Running]: the call's first progress, written before it
// returns, so the response's first byte is on its way before the tool
// starts, and the goroutine that writes one every cadence after it.
func (b *beat) start() {
	b.once.Do(func() {
		if b.token == nil || !b.send(0) {
			return
		}
		b.done = make(chan struct{})
		go b.run()
	})
}

// run writes a progress every cadence until the beat is stopped, the request
// ends or a write fails.
func (b *beat) run() {
	defer close(b.done)
	tick := time.NewTicker(b.every)
	defer tick.Stop()
	for n := 1; ; n++ {
		select {
		case <-b.quit:
			return
		case <-b.unread.Done():
			return
		case <-tick.C:
		}
		// A STOP THAT CAME WITH THE TICK WINS: the tool has returned, and its
		// answer is next.
		select {
		case <-b.quit:
			return
		default:
		}
		if !b.send(n) {
			return
		}
	}
}

// send writes progress n on the call's own response, reporting whether it
// could. One that could not means the response is gone, and nobody can read
// the answer any longer: [Call.Abandoned] is closed.
func (b *beat) send(n int) bool {
	if b.unread.Err() != nil {
		return false
	}
	err := b.session.NotifyProgress(b.ctx, &mcp.ProgressNotificationParams{
		ProgressToken: b.token, Progress: float64(n)})
	if err != nil {
		b.lose()
		return false
	}
	return true
}

// stop ends the beat and returns once it has, so nothing it writes comes after
// the answer; a beat never started is never started after it. Nil-safe: a
// server that keeps nothing going has no beat.
func (b *beat) stop() {
	if b == nil {
		return
	}
	b.once.Do(func() {})
	close(b.quit)
	if b.done != nil {
		<-b.done
	}
}
