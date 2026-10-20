// Package httpapitest serves an [http.Handler] to a provider's SDK in
// memory, for the cases that time a streamed response.
//
// # Why not a listener
//
// A streamed call is bounded by its silence ([httpapi.IdleWatchdog]), and a
// case proving that bound runs a server that pauses between events. Over
// httptest's real socket, what the watchdog saw between two reads was the
// server's pause PLUS however long the reading goroutine waited to be
// scheduled, so the anthropic and openai cases were each widened for it and
// could still fail either way under a race-instrumented suite on busy CPUs.
// Inside a testing/synctest bubble the only time that passes is the
// handler's own, but a socket is something the bubble cannot see: a read
// blocked on it is not durably blocked, and the clock never moves.
//
// So [Client] runs the handler on a goroutine of the caller's bubble and
// hands the SDK a response whose body is an [io.Pipe] the handler writes:
// every wait — the handler's pause, the reader's block on the pipe, the
// watchdog's timer — is on the bubble's clock, and an assertion can sit on
// the bound's edge. Each SDK takes it through its option.HTTPClient, which
// [Client] satisfies.
//
// [httpapi.IdleWatchdog]: github.com/crewlet/crewlet/internal/providers/llm/httpapi.IdleWatchdog
package httpapitest

import (
	"context"
	"io"
	"net/http"
	"sync"
)

// Client serves every request it is handed with its handler, in memory.
type Client struct {
	handler http.Handler
}

// NewClient is a client answering every request with h.
func NewClient(h http.Handler) *Client { return &Client{handler: h} }

// Do runs the handler on a goroutine of its own and answers once the handler
// has written its status, with the body still being written.
//
// THE REQUEST'S CONTEXT ENDS THE EXCHANGE, as a transport closes the
// connection under a cancelled request: before the status is written Do
// answers the context's error, and after it the body's next read fails with
// it, and so does the handler's next write. A handler that pauses should
// pause on the request's context, as a server's next write to a closed
// connection fails, because a goroutine still asleep when a synctest case
// ends is a bubble that cannot end.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	body, pipe := io.Pipe()
	w := &response{header: http.Header{}, body: pipe, written: make(chan struct{})}
	stop := context.AfterFunc(req.Context(), func() {
		_ = pipe.CloseWithError(req.Context().Err())
	})
	go func() {
		defer stop()
		c.handler.ServeHTTP(w, req)
		w.WriteHeader(http.StatusOK)
		// A handler that stopped BECAUSE the request ended has not ended
		// the body: the transport's connection was torn down under it, so
		// the reader is told the request's error rather than a clean end
		// it could take for the server's — whichever of this and the
		// context's own close runs first, since a pipe keeps the first
		// error it is closed with.
		_ = pipe.CloseWithError(req.Context().Err())
	}()
	select {
	case <-w.written:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return &http.Response{
		Status:        http.StatusText(w.status),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.sent,
		Body:          body,
		ContentLength: -1,
		Request:       req,
	}, nil
}

// response is the handler's side of one exchange.
type response struct {
	header  http.Header
	body    *io.PipeWriter
	once    sync.Once
	written chan struct{}
	// status and sent are set once, when the status is written, and read
	// only after written is closed.
	status int
	sent   http.Header
}

func (r *response) Header() http.Header { return r.header }

// WriteHeader sends the status and the headers set so far; a later call, or
// a header set after it, changes nothing, as on a real server.
func (r *response) WriteHeader(status int) {
	r.once.Do(func() {
		r.status, r.sent = status, r.header.Clone()
		close(r.written)
	})
}

func (r *response) Write(b []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return r.body.Write(b)
}

// Flush is a no-op: the pipe holds nothing back, since a write returns only
// once the reader has taken it.
func (*response) Flush() {}
