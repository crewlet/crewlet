package httprelay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// writer is the response a holder's handler writes: what [Relay.push] sends
// on as frames.
//
// IT BLOCKS ITS HANDLER once a frame's worth of body is waiting to be sent,
// and that is the point of it: the bytes leave this process only as fast as
// the entry's client takes them, so a large answer is never held here whole.
type writer struct {
	ctx context.Context

	mu sync.Mutex
	// change is closed and replaced on every change below, which both the
	// pusher (waiting for something to send) and a blocked Write (waiting
	// for room) wait on.
	change chan struct{}

	header http.Header
	// status is the status the handler committed, zero until it did, and
	// committed the header as it stood then: a handler's later edits to
	// its header map are not part of the response, as on a connection.
	status    int
	committed http.Header
	// buf is the body written and not yet sent.
	buf []byte
	// done says the handler has returned.
	done bool
}

var (
	_ http.ResponseWriter = (*writer)(nil)
	_ http.Flusher        = (*writer)(nil)
)

func newWriter(ctx context.Context) *writer {
	return &writer{ctx: ctx, change: make(chan struct{}), header: http.Header{}}
}

// signalLocked wakes everything waiting on a change.
func (w *writer) signalLocked() {
	close(w.change)
	w.change = make(chan struct{})
}

func (w *writer) Header() http.Header { return w.header }

func (w *writer) WriteHeader(code int) {
	// AN INFORMATIONAL STATUS IS NOT THE RESPONSE'S, and a relay has no
	// connection to send one ahead of it on.
	if code < http.StatusOK {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.commitLocked(code)
}

func (w *writer) commitLocked(code int) {
	if w.status != 0 {
		return
	}
	w.status, w.committed = code, w.header.Clone()
	w.signalLocked()
}

// errFinished is a write after the handler returned.
var errFinished = errors.New("httprelay: a write after the response ended")

func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.commitLocked(http.StatusOK)
	n := 0
	for len(p) > 0 {
		switch {
		case w.done:
			return n, errFinished
		case w.ctx.Err() != nil:
			return n, w.ctx.Err()
		}
		if room := MaxBody - len(w.buf); room > 0 {
			take := min(room, len(p))
			w.buf = append(w.buf, p[:take]...)
			p, n = p[take:], n+take
			w.signalLocked()
			continue
		}
		wait := w.change
		w.mu.Unlock()
		select {
		case <-wait:
		case <-w.ctx.Done():
		}
		w.mu.Lock()
	}
	return n, nil
}

// Flush commits the header. The body needs no flushing: every byte written is
// on offer to the pusher the moment it is written.
func (w *writer) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.commitLocked(http.StatusOK)
	w.signalLocked()
}

// FlushError is [writer.Flush] for [http.ResponseController].
func (w *writer) FlushError() error {
	w.Flush()
	return nil
}

// finish records that the handler returned.
func (w *writer) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.commitLocked(http.StatusOK)
	w.done = true
	w.signalLocked()
}

// failed answers a handler that panicked before it committed anything 500, as
// a server would; one that had committed is cut where it stopped.
func (w *writer) failed() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.header = http.Header{}
		w.commitLocked(http.StatusInternalServerError)
	}
}

// next waits for the next frame to send — the header once it is committed and
// wantHeader asks for it, the body written since, the end — or for beat to
// pass with nothing, which is a heartbeat's empty frame. It reports false once
// ctx is done.
func (w *writer) next(ctx context.Context, beat time.Duration, wantHeader bool) (frameRequest, bool) {
	timer := time.NewTimer(beat)
	defer timer.Stop()
	w.mu.Lock()
	for {
		if ctx.Err() != nil {
			w.mu.Unlock()
			return frameRequest{}, false
		}
		header := wantHeader && w.status != 0
		if header || len(w.buf) > 0 || w.done {
			var f frameRequest
			if header {
				f.Status, f.Header = w.status, w.committed
			}
			take := min(len(w.buf), MaxBody)
			if take > 0 {
				f.Body = bytes.Clone(w.buf[:take])
			}
			f.End = w.done && take == len(w.buf)
			w.mu.Unlock()
			return f, true
		}
		wait := w.change
		w.mu.Unlock()
		select {
		case <-wait:
		case <-timer.C:
			return frameRequest{}, true
		case <-ctx.Done():
		}
		w.mu.Lock()
	}
}

// sent drops the n bytes of body a frame carried, once the entry has them.
func (w *writer) sent(n int) {
	if n == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = w.buf[n:]
	if len(w.buf) == 0 {
		// THE BACKING ARRAY GOES WITH THE LAST BYTE, so a stream that once
		// carried a large answer does not hold its buffer for the rest of
		// its life.
		w.buf = nil
	}
	w.signalLocked()
}

// bytesReader is a request body over b, or none for an empty one.
func bytesReader(b []byte) io.Reader {
	if len(b) == 0 {
		return http.NoBody
	}
	return bytes.NewReader(b)
}
