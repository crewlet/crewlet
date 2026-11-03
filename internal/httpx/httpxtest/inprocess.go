package httpxtest

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

// InProcess is a client that answers every request with handler, run in this
// process on a goroutine of its own: no listener, no connection and no pool,
// so a test may run it inside a testing/synctest bubble (see the package
// doc).
//
// It answers as net/http's server and transport would, in what a caller can
// observe:
//
//   - the answer arrives AS IT IS WRITTEN. The response is handed back when
//     the handler first writes, flushes or returns — a WriteHeader alone
//     sends nothing, as the server sends nothing until its buffer is flushed
//     — and every later write reaches the body's reader as it is made. A
//     write returns once it has been read, which a socket's buffers would
//     only have delayed;
//   - the CALLER'S CONTEXT ends the call wherever it is: before the answer,
//     the round trip returns the context's cause; part-way through, the
//     body's next read does; and the handler's next write fails, as it would
//     on a connection the client dropped;
//   - the answer is DATED from the clock it runs on (the bubble's, inside
//     one), and an unset Content-Type is sniffed from the first write, as the
//     server does both;
//   - a response that carries no body (a HEAD, a 204, a 304) has none, and
//     a handler's write to a 204 or a 304 is refused with
//     http.ErrBodyNotAllowed.
//
// The handler is handed a copy of the request carrying what a server always
// gives one — a body, a host, its request URI and a remote address — since a
// RoundTripper may not edit the request it is handed.
//
// THE TEST DOES NOT END WHILE A HANDLER IT STARTED IS RUNNING: tb's cleanup
// waits for every one. Inside a bubble that is what lets a case end before
// its stand-in does — a handler still asleep when the case returns, behind a
// caller that gave up on it, is run to its end on the bubble's clock, where a
// bubble whose case had returned would declare it deadlocked instead. So tb
// is the bubble's own T when the client is used inside one. A handler that
// can never end — writing to a body its caller neither reads nor closes —
// fails the case rather than outliving it.
func InProcess(tb testing.TB, handler http.Handler) *http.Client {
	tb.Helper()
	p := &inProcess{handler: handler}
	tb.Cleanup(p.running.Wait)
	return &http.Client{Transport: p}
}

type inProcess struct {
	handler http.Handler
	// running counts the goroutines a round trip leaves behind it: the
	// handler, and the watch on its caller's context.
	running sync.WaitGroup
}

func (p *inProcess) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	in := req.Clone(ctx)
	if in.Body == nil {
		in.Body = http.NoBody
	}
	if in.Host == "" {
		in.Host = req.URL.Host
	}
	in.RequestURI = req.URL.RequestURI()
	// The documentation address net/http/httptest.NewRequest gives a
	// request: a server always sets one, and no caller here has a socket.
	in.RemoteAddr = "192.0.2.1:1234"

	body, pipe := io.Pipe()
	w := &pipeWriter{head: req.Method == http.MethodHead, header: http.Header{},
		pipe: pipe, committed: make(chan struct{})}
	finished := make(chan struct{})
	p.running.Go(func() {
		defer close(finished)
		// A RoundTripper closes the request's body, and may do so after it
		// has returned; the handler has read what it will by now.
		defer func() { _ = in.Body.Close() }()
		p.handler.ServeHTTP(w, in)
		w.commit(nil)
		_ = pipe.Close()
	})
	p.running.Go(func() {
		select {
		case <-ctx.Done():
			// The caller's cause is what its next read returns, as the
			// transport reports it; a close the handler's own end already
			// made stands.
			_ = pipe.CloseWithError(context.Cause(ctx))
		case <-finished:
		}
	})

	select {
	case <-w.committed:
		return w.response(req, body), nil
	case <-ctx.Done():
		// An answer that raced the caller's context is still the answer, as
		// the transport takes it (its roundTrip checks for one first).
		select {
		case <-w.committed:
			return w.response(req, body), nil
		default:
		}
		return nil, context.Cause(ctx)
	}
}

// pipeWriter is the handler's http.ResponseWriter, writing straight into the
// pipe the caller's body reads from. Every field but committed belongs to the
// handler's goroutine until committed is closed, and is only read after it.
type pipeWriter struct {
	head      bool
	header    http.Header
	status    int
	pipe      *io.PipeWriter
	once      sync.Once
	committed chan struct{}
	sent      http.Header // the header as it was when the response went out
}

func (w *pipeWriter) Header() http.Header { return w.header }

// WriteHeader records the status the answer will carry. An informational
// status is not the answer and is dropped, as a client's transport drops it
// on the way to the final one; a second final status is ignored, as the
// server ignores it.
func (w *pipeWriter) WriteHeader(code int) {
	if w.status != 0 || (code >= 100 && code < 200) {
		return
	}
	w.status = code
}

func (w *pipeWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if len(b) == 0 {
		return 0, nil
	}
	if !bodyAllowed(w.status) {
		return 0, http.ErrBodyNotAllowed
	}
	w.commit(b)
	if w.head {
		return len(b), nil
	}
	return w.pipe.Write(b)
}

// Flush sends the response if it has not gone yet; every write is already on
// its way to the reader as it is made.
func (w *pipeWriter) Flush() { w.commit(nil) }

func (w *pipeWriter) commit(first []byte) {
	w.once.Do(func() {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		if _, set := w.header["Content-Type"]; !set && len(first) > 0 {
			w.header.Set("Content-Type", http.DetectContentType(first))
		}
		if _, set := w.header["Date"]; !set {
			w.header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
		}
		w.sent = w.header.Clone()
		close(w.committed)
	})
}

func (w *pipeWriter) response(req *http.Request, body *io.PipeReader) *http.Response {
	res := &http.Response{
		Status:        strconv.Itoa(w.status) + " " + http.StatusText(w.status),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.sent,
		Body:          body,
		ContentLength: -1,
		Request:       req,
	}
	if n, err := strconv.ParseInt(w.sent.Get("Content-Length"), 10, 64); err == nil && n >= 0 {
		res.ContentLength = n
	}
	if w.head || !bodyAllowed(w.status) {
		// Nothing will be written into the pipe, and the handler's own end
		// closes it when it returns; the caller is handed what the
		// transport hands it for a response with no body.
		_ = body.Close()
		res.Body = http.NoBody
		if !bodyAllowed(w.status) {
			res.ContentLength = 0
		}
	}
	return res
}

// bodyAllowed is net/http's own rule for which statuses may carry a body.
func bodyAllowed(status int) bool {
	return status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified
}
