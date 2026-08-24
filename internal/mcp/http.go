package mcp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxLoggedErrorBody bounds how much of a failing HTTP response is logged.
// REASONED: an MCP error body is a JSON object of a few hundred bytes; 8 KiB
// covers a verbose one and refuses to put an HTML error page — or a gateway's
// debug dump — into the log stream.
const maxLoggedErrorBody = 8 << 10

// protocolVersionHeader is the header the streamable transport sends on every
// request after the handshake.
//
// Named here because this package supplies it. The SDK normally fills it in
// from a hook it reaches through an UNEXPORTED interface on its own connection
// type — a hook the annotation probe's wrapper cannot implement (see
// probe.go). Losing it silently would mean every request to a legacy remote
// server going out without the version header the spec says to send, and a
// server that enforces it answering 400 for reasons nothing in the engine
// could explain. So it is restored explicitly, from the negotiated version the
// session reports after Connect.
const protocolVersionHeader = "Mcp-Protocol-Version"

// newHTTPTransport builds the streamable transport and the round tripper that
// carries the seat's identity.
//
// DisableStandaloneSSE is on deliberately. The standalone GET stream exists to
// deliver server-initiated notifications — tools/prompts/resources list
// changed — and this engine registers no handler for any of them, so the
// stream would carry nothing anyone reads. It is also the other thing the
// probe's wrapper costs, so leaving it nominally enabled would be a lie about
// what the connection does.
func newHTTPTransport(spec Spec, log *slog.Logger) (sdk.Transport, *httpIdentity) {
	ident := &httpIdentity{
		base:    http.DefaultTransport,
		headers: maps.Clone(spec.Headers),
		server:  spec.Name,
		log:     log,
	}
	// Connect runs under the startup budget; everything after it under the
	// per-call one. See the deadline field.
	ident.setDeadline(spec.startupTimeout())
	return &sdk.StreamableClientTransport{
		Endpoint:             spec.URL,
		HTTPClient:           &http.Client{Transport: ident},
		DisableStandaloneSSE: true,
	}, ident
}

// httpIdentity is the round tripper for one remote server: it carries the
// configured headers, restores the protocol-version header, and surfaces an
// error body while it still exists.
type httpIdentity struct {
	base    http.RoundTripper
	headers map[string]string
	server  string
	log     *slog.Logger

	// protocolVersion is set once, after the handshake settles. Atomic
	// because the session's requests run on their own goroutines.
	protocolVersion atomic.Pointer[string]

	// deadline bounds ONE HTTP request, and it is here because nothing above
	// it can do the job.
	//
	// The context a caller passes to Connect or CallTool bounds the JSON-RPC
	// CALL — the wait for an answer — but not the HTTP request underneath it:
	// the transport writes from the connection's own goroutine, with the
	// connection's context. So a caller's deadline expires, the call errors,
	// and then closing the session BLOCKS waiting for the in-flight request
	// nobody bounded. Measured against an endpoint that accepts a connection
	// and never answers: a 300ms startup budget produced a 10.3s connect.
	//
	// Cannot be an http.Client.Timeout, which covers the body read too and
	// would sever a streaming tool result mid-flight. Cannot be a fixed
	// ResponseHeaderTimeout either: a non-streaming server sends its headers
	// with the answer, so the legitimate wait for headers IS the tool's own
	// budget. So it follows the phase — the startup budget during connect,
	// the per-call budget afterwards.
	deadline atomic.Int64 // time.Duration
}

// setProtocolVersion records the negotiated version for subsequent requests.
func (h *httpIdentity) setProtocolVersion(v string) {
	if v != "" {
		h.protocolVersion.Store(&v)
	}
}

// setDeadline switches the per-request ceiling.
func (h *httpIdentity) setDeadline(d time.Duration) { h.deadline.Store(int64(d)) }

func (h *httpIdentity) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	var cancel context.CancelFunc
	if d := time.Duration(h.deadline.Load()); d > 0 {
		ctx, cancel = context.WithTimeout(ctx, d)
	}
	// Clone: RoundTrippers must not mutate the request they are handed.
	req = req.Clone(ctx)
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get(protocolVersionHeader) == "" {
		if v := h.protocolVersion.Load(); v != nil {
			req.Header.Set(protocolVersionHeader, *v)
		}
	}

	resp, err := h.base.RoundTrip(req)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}
	if cancel != nil {
		// The deadline has to outlive RoundTrip — the body is read after it
		// returns, and an SSE body is read for as long as the tool runs — so
		// the cancel rides on the body and fires when the reader is done with
		// it. Dropping it here instead would cut every response short.
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	}
	if resp.StatusCode < 400 {
		return resp, nil
	}

	// The SDK surfaces an HTTP failure only after its task group unwinds, by
	// which point the body is closed and gone — and the body is where a
	// remote server says WHY (a scope missing, a token expired, a workspace
	// suspended). Read it here, log it, and hand the SDK an identical one.
	//
	// The REQUEST body is deliberately not logged, which differs from the
	// Python client. It is the JSON-RPC call, and its params are tool
	// arguments an agent composed — which can carry a credential it was given
	// to pass along. There is no redaction pass on this side of the engine
	// yet, and a log line is a permanent place to put a secret.
	//
	// The body is read WHOLE and only a bounded slice is logged. Handing the
	// SDK a truncated body would turn a server's clear 403 into a JSON parse
	// error, which is a worse diagnostic than the one this exists to produce.
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close() // also releases the deadline above
	if readErr != nil {
		h.log.Error("http_error", "server", h.server, "status_code", resp.StatusCode,
			"method", req.Method, "url", req.URL.String(), "body_error", readErr.Error())
	} else {
		h.log.Error("http_error", "server", h.server, "status_code", resp.StatusCode,
			"method", req.Method, "url", req.URL.String(),
			"response_body", boundedBody(body))
	}
	// The body was consumed to log it; hand the SDK an identical one. The
	// deadline is already released, which is correct: this response is
	// complete.
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

// cancelOnClose releases a per-request deadline when the reader is finished
// with the body, which for a streamed response is long after RoundTrip
// returned.
type cancelOnClose struct {
	io.ReadCloser
	once   sync.Once
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.cancel)
	return err
}

func boundedBody(body []byte) string {
	switch {
	case len(body) == 0:
		return "(empty)"
	case len(body) > maxLoggedErrorBody:
		return string(body[:maxLoggedErrorBody]) + truncationMarker
	default:
		return string(body)
	}
}
