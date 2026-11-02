package mcp

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// maxLoggedErrorBody bounds how much of a failing HTTP response is logged.
// REASONED: an MCP error body is a JSON object of a few hundred bytes; 8 KiB
// covers a verbose one and refuses to put an HTML error page — or a gateway's
// debug dump — into the log stream.
const maxLoggedErrorBody = 8 << 10

// maxBufferedErrorBody bounds how much of a failing response this process
// holds in memory to log and replay.
//
// The read has to be WHOLE to be useful — handing the SDK a truncated body
// turns a server's clear 403 into a JSON parse error — but "whole" was
// unbounded, which let a remote server choose this process's allocation size.
// It was the only unbounded io.ReadAll left in the tree, eight lines below a
// constant whose entire purpose is bounding this same body.
//
// A MEBIBYTE, which is 128 times the logged slice above and far past any real
// MCP error object; what it actually refuses is a gateway streaming an
// unbounded HTML page under a 5xx. Past it nothing further is buffered: the
// prefix already read is handed back in front of the still-open body, so the
// SDK reads the same bytes it would have, and the response is neither
// truncated nor held.
const maxBufferedErrorBody = 1 << 20

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
// carries the seat's identity, over d's transport ([httpx.Transport] when it
// names none).
//
// DisableStandaloneSSE is on deliberately. The standalone GET stream exists to
// deliver server-initiated notifications — tools/prompts/resources list
// changed — and this engine registers no handler for any of them, so the
// stream would carry nothing anyone reads. It is also the other thing the
// probe's wrapper costs, so leaving it nominally enabled would be a lie about
// what the connection does.
//
// A REDIRECT IS FOLLOWED WITHIN THE CONFIGURED ORIGIN ALONE ([sameOrigin]):
// see [httpIdentity.redirect].
func newHTTPTransport(spec Spec, log *slog.Logger, d dialing) (sdk.Transport, *httpIdentity) {
	base := d.base
	if base == nil {
		base = httpx.Transport()
	}
	ident := &httpIdentity{
		base:    base,
		headers: maps.Clone(spec.Headers),
		bearer:  spec.Bearer,
		origin:  originOf(spec.URL),
		server:  spec.Name,
		log:     log,
		trace:   d.trace,
	}
	// Connect runs under the startup budget; everything after it under the
	// per-call one. See the deadline field.
	ident.setDeadline(spec.startupTimeout())
	return &sdk.StreamableClientTransport{
		Endpoint:             spec.URL,
		HTTPClient:           &http.Client{Transport: ident, CheckRedirect: ident.redirect},
		DisableStandaloneSSE: true,
	}, ident
}

// dialing is how one connection to a server is made, beyond its spec: the
// transport an HTTP connection rides and the trace its requests report to,
// both unset for a server the engine runs (the shared transport, no trace). A
// test sets them, to time each stage of a connection of its own
// ([Test]).
type dialing struct {
	base  http.RoundTripper
	trace *httptrace.ClientTrace
}

// maxRedirects bounds the redirects one request follows within its server's
// origin: net/http's own default, which no MCP server needs more than one of
// (a trailing slash, a path the server moved).
const maxRedirects = 10

// ErrRedirectedAway is a server that answered with a redirect to an address
// outside its configured origin, which the engine does not follow.
var ErrRedirectedAway = errors.New("mcp: the server redirected to another origin")

// redirect is the connection's redirect policy: a redirect within the
// configured origin (scheme, host and port) is followed, and one to anywhere
// else is refused.
//
// REFUSED, NOT FOLLOWED THROUGH THE GUARD. The configured address is the
// operator's, dialled as it stands whatever network it is on, and carries the
// headers configured for it; an address a redirect names is the server's
// (ADR-0072). Followed, a 307 or a 308 would carry the JSON-RPC body to it,
// and a hop the guard admitted would still reach an origin nobody configured.
// Refused, the request fails with a sentence naming the origin it was sent
// to, which an operator who trusts it writes as the server's address. The
// round tripper sets the configured headers on a request to the configured
// origin alone ([httpIdentity.RoundTrip]), so no credential leaves it either
// way.
func (h *httpIdentity) redirect(req *http.Request, via []*http.Request) error {
	if !sameOrigin(req.URL, h.origin) {
		return classify(FailNoEndpoint, fmt.Errorf("%w: it answered with a redirect to %s, which is not "+
			"the server's address. Set the server's url to that address if it is the one to use",
			ErrRedirectedAway, redact.URL(req.URL.String())))
	}
	if len(via) >= maxRedirects {
		return fmt.Errorf("mcp: the server redirected %d times without answering", len(via))
	}
	return nil
}

// origin is a URL's scheme, host and port, the port written out where the
// URL leaves it to the scheme.
type origin struct{ scheme, host, port string }

// originOf is raw's origin, the zero origin for an address that does not
// parse, which no request's origin equals.
func originOf(raw string) origin {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return origin{}
	}
	return originOfURL(u)
}

func originOfURL(u *url.URL) origin {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return origin{scheme: strings.ToLower(u.Scheme), host: strings.ToLower(u.Hostname()), port: port}
}

// SameOrigin reports whether two addresses are on one origin: the same scheme,
// host and port, the port read from the scheme where an address leaves it
// out. False for an address that does not parse.
func SameOrigin(a, b string) bool {
	u, err := url.Parse(a)
	return err == nil && u.Host != "" && sameOrigin(u, originOf(b))
}

// sameOrigin reports whether u is at o: the same scheme, host and port.
func sameOrigin(u *url.URL, o origin) bool {
	return o != (origin{}) && originOfURL(u) == o
}

// httpIdentity is the round tripper for one remote server: it carries the
// configured headers, restores the protocol-version header, and surfaces an
// error body while it still exists.
type httpIdentity struct {
	base    http.RoundTripper
	headers map[string]string
	// bearer answers the access token of a server signed in to with OAuth
	// ([Spec.Bearer]), nil for any other: set per request, on a request to
	// the configured origin alone, as the headers are.
	bearer BearerSource
	// origin is the configured address's: the one origin a request is sent
	// the configured headers to.
	origin origin
	server string
	log    *slog.Logger
	// trace, when set, is told of each request's connection
	// ([dialing.trace]).
	trace *httptrace.ClientTrace

	// protocolVersion is set once, after the handshake settles. Atomic
	// because the session's requests run on their own goroutines.
	protocolVersion atomic.Pointer[string]

	// failed is the server's latest answer when it was not a success, nil
	// when its latest answer was one: what a failed start and an ended
	// session are classed by ([client.startClass], [client.deathClass]).
	// CLEARED BY A SUCCESS, so a 429 the session rode out an hour ago is not
	// the class of a session that ends for another reason now.
	failed atomic.Pointer[answerFailure]

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
	if h.trace != nil {
		ctx = httptrace.WithClientTrace(ctx, h.trace)
	}
	// Clone: RoundTrippers must not mutate the request they are handed.
	req = req.Clone(ctx)
	// THE CONFIGURED HEADERS GO TO THE CONFIGURED ORIGIN ALONE. The redirect
	// policy refuses every other origin before a request to it is made; this
	// is the same rule where the headers are set, so no path that reaches
	// this tripper can carry them elsewhere.
	configured := sameOrigin(req.URL, h.origin)
	if configured {
		for k, v := range h.headers {
			req.Header.Set(k, v)
		}
	}
	if req.Header.Get(protocolVersionHeader) == "" {
		if v := h.protocolVersion.Load(); v != nil {
			req.Header.Set(protocolVersionHeader, *v)
		}
	}
	resp, sent, err := h.send(ctx, req, configured)
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
		if webPage(resp) {
			// A LOGIN PAGE, A PROXY OR A CAPTIVE PORTAL answers 200 with
			// HTML, and the SDK's error for it is a parse failure.
			h.failed.Store(&answerFailure{status: resp.StatusCode, class: FailNotMCP})
		} else {
			h.failed.Store(nil)
		}
		return resp, nil
	}

	// The SDK surfaces an HTTP failure only after its task group unwinds, by
	// which point the body is closed and gone — and the body is where a
	// remote server says WHY (a scope missing, a token expired, a workspace
	// suspended). Read it here, log it, and hand the SDK an identical one.
	//
	// The REQUEST body is deliberately not logged. It is the JSON-RPC
	// call, and its params are tool arguments an agent composed — which can carry a credential it was given
	// to pass along. There is no redaction pass on this side of the engine
	// yet, and a log line is a permanent place to put a secret.
	//
	// Read whole up to maxBufferedErrorBody, and only a bounded slice is
	// logged. Handing the SDK a truncated body would turn a server's clear
	// 403 into a JSON parse error, which is a worse diagnostic than the one
	// this exists to produce — so past the cap the body is not truncated
	// either: what has been read is handed back in FRONT of the still-open
	// original, and the SDK reads exactly the bytes it would have.
	//
	// One byte over the cap is read on purpose, so a body sitting exactly on
	// it is not mistaken for an overflowing one.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBufferedErrorBody+1))
	// THE ADDRESS REDACTED: a server's address can carry its key in the query,
	// which the log's own pass does not recognise by shape.
	if readErr != nil {
		h.log.Error("http_error", "server", h.server, "status_code", resp.StatusCode,
			"method", req.Method, "url", redact.URL(req.URL.String()), "body_error", readErr.Error())
	} else {
		// AN ACCESS TOKEN THE SERVER ECHOED IS WITHHELD from the line: the
		// engine renews it on its own, and no log is a place to keep one.
		h.log.Error("http_error", "server", h.server, "status_code", resp.StatusCode,
			"method", req.Method, "url", redact.URL(req.URL.String()),
			"response_body", withholdToken(boundedBody(body), sent))
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	h.failed.Store(&answerFailure{status: resp.StatusCode, class: classOfStatus(resp.StatusCode,
		bearerChallenge(challenge), req.Header.Get("Authorization") != "",
		bytes.Contains(bytes.ToLower(body), []byte("protocol version"))),
		challenge: textcut.Bytes(challenge, maxChallenge)})
	if len(body) > maxBufferedErrorBody {
		// NOT CLOSED, and that is what keeps the deadline correct: the
		// rest of the body is still to be read, and cancelOnClose fires
		// when the SDK closes it. Closing here would cut the response the
		// SDK is about to read.
		rest := resp.Body
		resp.Body = readCloser{
			Reader: io.MultiReader(bytes.NewReader(body), rest),
			Closer: rest,
		}
		return resp, nil
	}
	_ = resp.Body.Close() // also releases the deadline above
	// The body was consumed to log it; hand the SDK an identical one. The
	// deadline is already released, which is correct: this response is
	// complete.
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

// send carries one request to the server, with the access token of a server
// signed in to with OAuth where it goes to the configured origin, and answers
// the response and the token it carried ("" for none).
//
// A 401 TO A TOKEN IS ONE RENEWAL AND ONE RETRY. The token may have been
// revoked or expired before its stated life ran out, so the source is told it
// was refused ([BearerSource.Rejected]) and the request is sent once more with
// what it answers; a second refusal is the server's answer, classed as any
// 401 is. A token the source cannot give classes the server
// [FailNeedsSignIn] (or what its error says) and sends nothing.
func (h *httpIdentity) send(ctx context.Context, req *http.Request, configured bool) (
	*http.Response, string, error) {

	if h.bearer == nil || !configured {
		resp, err := h.base.RoundTrip(req)
		return resp, "", err
	}
	token, err := h.bearer.Token(ctx)
	if err != nil {
		h.failed.Store(&answerFailure{class: cmp.Or(ClassOf(err), FailNeedsSignIn)})
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized ||
		!bearerChallenge(resp.Header.Get("WWW-Authenticate")) {
		return resp, token, err
	}
	// THE REFUSAL STANDS where no renewed token came back, read and classed
	// below; why it could not be renewed is the source's to have said.
	renewed, renewErr := h.bearer.Rejected(ctx, token)
	retry, ok := replay(ctx, req)
	if renewErr == nil && ok && renewed != token {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBufferedErrorBody))
		_ = resp.Body.Close()
		retry.Header.Set("Authorization", "Bearer "+renewed)
		resp, err = h.base.RoundTrip(retry)
		return resp, renewed, err
	}
	return resp, token, nil
}

// replay is req again, with its body read afresh: false for a request whose
// body cannot be (the SDK sends every POST from a byte slice, which can).
func replay(ctx context.Context, req *http.Request) (*http.Request, bool) {
	again := req.Clone(ctx)
	if req.Body == nil || req.Body == http.NoBody {
		return again, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	again.Body = body
	return again, true
}

// withholdToken is text with every occurrence of token replaced, for a line
// that quotes what a server answered a request that carried it.
func withholdToken(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "[withheld]")
}

// answerFailure is one answer a remote server gave that was not a success:
// its status, and the class it is read as.
type answerFailure struct {
	status int
	class  FailureClass
	// challenge is the answer's WWW-Authenticate header, cut to
	// [maxChallenge]: where a server that wants a sign-in says where its
	// protected-resource metadata is (RFC 9728), which an address check
	// reads ([CheckAddress]).
	challenge string
}

// maxChallenge bounds the WWW-Authenticate header kept from a failed answer.
// REASONED: a Bearer challenge names a realm, a scope list, an error and a
// metadata address, a few hundred bytes; 2 KiB keeps a long scope list whole
// and refuses a server's choice of how much this process holds.
const maxChallenge = 2 << 10

// webPage reports whether resp is an HTML page: what a login page, a proxy or
// a captive portal answers in front of a server.
func webPage(resp *http.Response) bool {
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return err == nil && media == "text/html"
}

// bearerChallenge reports whether a WWW-Authenticate header challenges for a
// Bearer token: what a server that wants an OAuth sign-in answers a request
// that carried none (RFC 6750, and the MCP authorization specification).
func bearerChallenge(header string) bool {
	scheme, _, _ := strings.Cut(strings.TrimSpace(header), " ")
	return strings.EqualFold(scheme, "bearer")
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

// readCloser reads from one place and closes another — the shape needed to
// put a buffered prefix in front of a body whose Close still has to reach the
// original, so the request's deadline is released when the SDK is done.
type readCloser struct {
	io.Reader
	io.Closer
}

func boundedBody(body []byte) string {
	switch {
	case len(body) == 0:
		return "(empty)"
	case len(body) > maxLoggedErrorBody:
		// On a character boundary: a byte slice splits whatever rune
		// straddles the cap, and the log's encoder writes U+FFFD for it.
		return textcut.Bytes(string(body), maxLoggedErrorBody) + truncationMarker
	default:
		return string(body)
	}
}
