package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrStalled is a streamed response that went silent for longer than its idle
// bound. It also answers errors.Is(err, context.DeadlineExceeded), so
// [FromTransport] classifies it as a timeout: retryable on the next member of
// the chain, and never a reason to bench the key.
var ErrStalled = errors.New("stream went silent")

// IdleWatchdog bounds a STREAMED call by its silence rather than its length.
//
// A total deadline is the wrong bound for a stream. A round on a model that
// thinks at a high effort can write for many minutes, and every one of those
// minutes is the model working: a total bound sized for an ordinary answer
// kills exactly the rounds that were doing the most, half-way through, and a
// bound sized for those rounds lets a connection that died at the first byte
// hold a seat for as long. What distinguishes a dead stream from a long one is
// that a live one keeps SAYING something — the vendors send keep-alive events
// while the model thinks — so the bound is the longest gap between two reads of
// the body, from the moment the request is sent.
//
// It watches BYTES, not decoded events: the SDKs swallow the keep-alive events
// before a caller sees one, so a watchdog reset per event would declare a
// thinking model dead while its connection was demonstrably alive.
//
// One per attempt. Start it, run the request on the context it returns with
// [IdleWatchdog.Body] around the response body (each backend wraps it from the
// SDK's own middleware hook, [IdleWatchdog.Middleware]), and pass the
// attempt's error through [IdleWatchdog.Err].
type IdleWatchdog struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
	idle   time.Duration
	stall  error
}

// WatchIdle derives the attempt's context from ctx — the caller's, cancelled
// as well once idle passes with nothing read — and the watchdog that does it.
// A non-positive idle is a caller's bug — every stream would be declared dead
// at once — so it panics rather than guess.
func WatchIdle(ctx context.Context, idle time.Duration) (context.Context, *IdleWatchdog) {
	if idle <= 0 {
		panic(fmt.Sprintf("httpapi.WatchIdle: idle bound must be positive, got %v", idle))
	}
	wctx, cancel := context.WithCancelCause(ctx)
	w := &IdleWatchdog{
		ctx:    wctx,
		cancel: cancel,
		idle:   idle,
		// Both sentinels: ErrStalled names what happened, DeadlineExceeded
		// is what every classifier already reads as a timeout.
		stall: fmt.Errorf("%w: nothing arrived for %s: %w", ErrStalled, idle, context.DeadlineExceeded),
	}
	w.timer = time.AfterFunc(idle, func() { cancel(w.stall) })
	return wctx, w
}

// Body wraps a response body so every read that returns data resets the
// bound.
func (w *IdleWatchdog) Body(body io.ReadCloser) io.ReadCloser {
	return &watchedBody{ReadCloser: body, w: w}
}

// Middleware is the shape both SDKs' middleware hooks take (option.Middleware
// in each), written once: it hands the request on and wraps the body of
// whatever response comes back.
func (w *IdleWatchdog) Middleware(req *http.Request, next func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	res, err := next(req)
	if res != nil && res.Body != nil {
		res.Body = w.Body(res.Body)
	}
	return res, err
}

// Stop disarms the watchdog and releases its context. Call it once the
// attempt is over, success or failure.
func (w *IdleWatchdog) Stop() {
	w.timer.Stop()
	w.cancel(context.Canceled)
}

// Err is the attempt's error as the caller should see it: the stall, when the
// watchdog is what ended the attempt — the SDK only reports the cancelled
// context it caused, which reads as the caller giving up — and err unchanged
// otherwise, nil included. A cancellation that came from the CALLER's own
// context is the caller's, and is never reported as a stall.
func (w *IdleWatchdog) Err(err error) error {
	if err != nil && errors.Is(context.Cause(w.ctx), ErrStalled) {
		// The SDK's own error rides as TEXT, not wrapped: it answers
		// errors.Is(context.Canceled), which every classifier reads as the
		// caller's own cancellation.
		return fmt.Errorf("%w (%s)", w.stall, err.Error())
	}
	return err
}

type watchedBody struct {
	io.ReadCloser
	w *IdleWatchdog
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.w.timer.Reset(b.w.idle)
	}
	return n, err
}
