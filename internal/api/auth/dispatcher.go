package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/modelnote"
)

// HOW AN ASSISTANT TOOL CALL IS SENT: in process, through the very handler the
// API port serves (ADR-0050).
//
// # In process, and why not a loopback call
//
// A loopback request to api.port would make the grant a network credential: a
// local process or a proxy log could capture it, and it would depend on the
// bind address, TLS and the public listener's partition, which differ on
// exactly the hardened deployment that binds loopback behind a proxy. Called in
// process, the request meets the same page policy, canonical path, guard,
// cross-site check, drain gate, route and authority row as a click, and the
// grant never crosses a socket.
//
// # What the request carries, and what it does not
//
// The grant as its bearer, the in-process marker only this file sets, the
// body as JSON, the operation key of an approved write, and the caller's trace
// span. NO cookie and NO Origin, so the cross-site rule passes it as the bearer
// request it is and nothing ambient authenticates it; and NOTHING ELSE of the
// caller's context: the request is built on a fresh one carrying only the
// caller's cancellation, deadline and span, so no value the turn's own request
// was resolved with (its principal, its session, a Tier A marker) can reach a
// handler beside the one the guard composes.

// ErrForeignGrant reports a grant these delegations did not mint. The
// orchestrator holds only [exec.Grant]; anything else it hands back is refused
// before a request is built.
var ErrForeignGrant = errors.New("auth: the grant was not minted by this node's assistant delegations")

// ErrDispatcherUnbound reports a dispatcher no API handler has been bound to.
var ErrDispatcherUnbound = errors.New("auth: the assistant's dispatcher is bound to no API handler")

// Dispatcher serves an assistant tool call through the API's own handler, in
// process.
type Dispatcher struct {
	delegations *Delegations

	mu      sync.RWMutex
	handler http.Handler
}

var _ exec.Dispatcher = (*Dispatcher)(nil)

// NewDispatcher builds the dispatcher for the grants d mints.
func NewDispatcher(d *Delegations) *Dispatcher { return &Dispatcher{delegations: d} }

// Bind installs the handler every call is served by: the API's own, once, from
// api.New, so the chain a call meets is the chain a click meets.
func (d *Dispatcher) Bind(h http.Handler) error {
	switch {
	case d.delegations == nil:
		return errors.New("auth: the assistant's dispatcher has no delegations to " +
			"send grants for")
	case h == nil:
		return errors.New("auth: the assistant's dispatcher was bound to no handler")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handler != nil {
		return errors.New("auth: the assistant's dispatcher is already bound to a handler")
	}
	d.handler = h
	return nil
}

// Delegations is the minter whose grants this dispatcher sends, for the wiring
// that checks the two belong together.
func (d *Dispatcher) Delegations() *Delegations { return d.delegations }

// Do sends r through the API's handler as the person g was derived for, and
// answers what the chain answered, read to at most [exec.MaxResponseBytes], or
// [exec.PreviewMaxResponseBytes] for a preview's read ([exec.Request.ReadLimit]).
//
// An error is a request that could not be sent at all: a grant these
// delegations did not mint, a handler not yet bound, a request that is not
// one, or a context already done. Every answer the chain gave, a refusal
// included, is a [exec.Response].
func (d *Dispatcher) Do(ctx context.Context, g exec.Grant, r exec.Request) (exec.Response, error) {
	held, ok := g.(*grant)
	if !ok || held == nil || held.issuer == nil || held.issuer != d.delegations {
		return exec.Response{}, ErrForeignGrant
	}
	d.mu.RLock()
	h := d.handler
	d.mu.RUnlock()
	if h == nil {
		return exec.Response{}, ErrDispatcherUnbound
	}
	if err := ctx.Err(); err != nil {
		return exec.Response{}, err
	}
	u, err := r.URL()
	if err != nil {
		return exec.Response{}, fmt.Errorf("auth: an assistant call: %w", err)
	}
	if len(r.Body) > AssistMaxBody {
		return exec.Response{}, fmt.Errorf("auth: an assistant call's body is %d bytes, "+
			"over the %d a request may carry", len(r.Body), AssistMaxBody)
	}

	reqCtx, cancel := detached(ctx)
	defer cancel()
	// THE MODEL'S HALF OF A REFUSAL, heard in process only: the route
	// answers the person's words, and tells the tool's sentence here for
	// the model that made this request (internal/modelnote).
	reqCtx, note := modelnote.Expect(reqCtx)
	req, err := http.NewRequestWithContext(withInProcess(reqCtx), r.Method, u.String(),
		bytes.NewReader(r.Body))
	if err != nil {
		return exec.Response{}, fmt.Errorf("auth: build an assistant call: %w", err)
	}
	req.RequestURI = u.RequestURI()
	req.RemoteAddr = "inproc"
	req.Header.Set("Authorization", "Bearer "+held.token)
	if len(r.Body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.IdempotencyKey != "" {
		req.Header.Set(opkey.Header, r.IdempotencyKey)
	}
	if r.IfMatch != "" {
		if !exec.ValidEntityTag(r.IfMatch) {
			return exec.Response{}, fmt.Errorf("auth: an assistant call's If-Match %q is "+
				"not one strong entity tag", r.IfMatch)
		}
		req.Header.Set("If-Match", r.IfMatch)
	}
	if r.IfNoneMatch != "" {
		if r.IfNoneMatch != exec.AnyRepresentation || r.IfMatch != "" {
			return exec.Response{}, fmt.Errorf("auth: an assistant call's If-None-Match %q "+
				"is not the create-only %q, or rides beside an If-Match", r.IfNoneMatch,
				exec.AnyRepresentation)
		}
		req.Header.Set("If-None-Match", r.IfNoneMatch)
	}

	out := newCapture(r.Bound())
	if err := serve(reqCtx, h, out, req); err != nil {
		return exec.Response{}, err
	}
	resp := out.response()
	resp.ModelDetail = note.Said()
	return resp, nil
}

// detached is a fresh context carrying only ctx's cancellation, deadline and
// trace span: nothing ctx's own request was resolved with.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	base := trace.ContextWithSpan(context.Background(), trace.SpanFromContext(ctx))
	cancelDeadline := context.CancelFunc(func() {})
	if deadline, ok := ctx.Deadline(); ok {
		base, cancelDeadline = context.WithDeadline(base, deadline)
	}
	out, cancel := context.WithCancelCause(base)
	stop := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
	return out, func() { stop(); cancel(nil); cancelDeadline() }
}

// serve runs h for one in-process request, and turns a handler's panic into an
// error, as net/http's server turns one into a closed connection: in process,
// a panic would otherwise end the turn's goroutine and the engine with it. ctx
// is the request's own context, the one a panic is logged under.
func serve(ctx context.Context, h http.Handler, w http.ResponseWriter, r *http.Request) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.ErrorContext(ctx, "api_assist_handler_panicked",
				"route", r.URL.Path, "panic", fmt.Sprint(recovered))
			err = fmt.Errorf("auth: the handler for %s panicked", r.URL.Path)
		}
	}()
	h.ServeHTTP(w, r)
	return nil
}

// capture is an in-memory [http.ResponseWriter] that keeps at most limit
// bytes of the body and says when it cut one.
type capture struct {
	header    http.Header
	status    int
	body      bytes.Buffer
	limit     int
	truncated bool
}

func newCapture(limit int) *capture { return &capture{header: http.Header{}, limit: limit} }

func (c *capture) Header() http.Header { return c.header }

func (c *capture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

// Write keeps what fits and reports every byte written, so a handler is never
// told its answer failed because the reader stops listening early.
func (c *capture) Write(p []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	if room := c.limit - c.body.Len(); len(p) > room {
		c.body.Write(p[:max(room, 0)])
		c.truncated = true
		return len(p), nil
	}
	c.body.Write(p)
	return len(p), nil
}

func (c *capture) response() exec.Response {
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	return exec.Response{Status: status, Header: c.header.Clone(),
		Body: bytes.Clone(c.body.Bytes()), Truncated: c.truncated}
}
