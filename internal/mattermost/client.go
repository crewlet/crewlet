package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// The REST client.
//
// # Retrying is a question about SIDE EFFECTS, not about status codes
//
// A failure is retryable only when repeating the request cannot repeat an
// effect. Two things prove that: a status that says the request was rejected
// BEFORE it was processed, and a transport failure raised before any byte
// left this process. Everything else — a proxy's 502, a read timeout, a
// server that hung up mid-response — leaves it unknowable whether the
// request was applied and only the answer lost.
//
// That distinction is not academic. It is the difference between a read
// timeout on a token mint being harmless and it minting a SECOND access
// token, live on the account, whose value the caller that would have
// persisted or revoked it never saw.
//
// # A redirect is refused, never followed
//
// Go's client follows a 301, 302 or 303 by resending the request as a GET,
// without its body (net/http, redirectBehavior). For an API client that
// turns every write into a read of the same path: a typing indicator or a
// bot's disable fails on a route that takes no GET, and a bot's rename — PUT
// /bots/{id} — arrives as GET /bots/{id}, which Mattermost answers 200 with
// the unchanged bot, so the provisioner reported a rename that never
// happened, and a teardown a marker it never wrote. And the address is
// shared: the agents' Mattermost MCP server dials the same url, and its HTTP
// client follows no redirect at all, so an engine that followed one would
// connect and hear every message while its agents could not answer any.
//
// A server redirects the API only when the url is not the address it serves
// it at — Mattermost sends a request outside its Site URL's path into that
// path ([redirected]), a proxy sends http to https — so the redirect is
// answered as the misconfiguration it is: an error on every call, naming the
// address the server sent the request to.

// RetryStatuses are worth a second try at all: 429 is the rate limiter, and
// 502/503/504 are a proxy or a server mid-restart — which is the ordinary
// state of a container that is up but still migrating. Every other 4xx is
// the caller's fault and every other 5xx will not change on a second try.
var RetryStatuses = []int{429, 502, 503, 504}

// rejectedBeforeProcessing are the statuses that PROVE nothing happened. A
// rate-limited request never reached the handler, so repeating it cannot
// repeat a side effect — which makes 429 safe for any method, including the
// ones that mint credentials. A 502 from a proxy says nothing of the sort.
var rejectedBeforeProcessing = []int{429}

// RetryBudget is the total wall clock one call may spend waiting out
// retries.
//
// Ten seconds absorbs a proxy hiccup, a brief rate limit and a server
// finishing a restart, and deliberately stops short of waiting out an
// outage: every caller here is already on a retry loop of its own — the
// socket fleet's reconnect backoff, the scheduler, an operator re-running a
// command — so a longer budget would not rescue anything. It would turn one
// slow call into a wedged one.
const RetryBudget = 10 * time.Second

// DefaultTimeout bounds a single attempt.
//
// Thirty seconds, against an API whose slowest ordinary call is a channel
// backfill over a reconnect gap. Shorter starts failing that legitimately;
// longer means a hung server holds a socket-fleet goroutine past the point
// the reconnect would have recovered it.
const DefaultTimeout = 30 * time.Second

// DefaultTypingThrottle is how often a typing indicator may be re-asserted
// when the server does not say.
//
// Mattermost's own default, and the server enforces it: sending faster is
// not merely wasteful, it is rejected. The live value is read from the
// server's client config at connect, because an operator can change it.
const DefaultTypingThrottle = 5 * time.Second

// Error is a failed API call, carrying enough to be actionable without
// turning on request logging.
type Error struct {
	Method string
	Path   string
	Status int
	// Message is the server's own error text where it sent one. Mattermost
	// returns a structured error whose `message` is written for a person,
	// so surfacing it turns "500 on /users/me" into "Invalid session".
	Message string

	// retryAfter is what the server asked for, when it asked. Honouring it
	// is the difference between waiting out a rate limit and extending it.
	retryAfter time.Duration
}

func (e *Error) Error() string {
	s := fmt.Sprintf("mattermost: %s %s: %d", e.Method, e.Path, e.Status)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Status reports the HTTP status of err, or 0 when it was not an API error.
func Status(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// Client is one authenticated Mattermost session.
//
// One per SEAT, not one per process: every agent has its own bot account,
// and a shared client would post every agent's messages as one identity —
// which is the whole thing a company of agents is not.
type Client struct {
	base  string
	token string
	http  *http.Client
	now   func() time.Time
}

// ClientOptions configure a [Client].
type ClientOptions struct {
	// URL is the instance address; it is normalised.
	URL string

	// Token authenticates as one bot. Empty is valid for the handful of
	// endpoints that need no session — the client config read the doctor
	// uses to compare Site URLs.
	Token string

	// HTTP is the transport; nil takes one with [DefaultTimeout]. Its
	// redirect policy is not used: the client follows no redirect.
	HTTP *http.Client

	// Now is the clock, injectable so a test need not wait out a backoff.
	Now func() time.Time
}

// NewClient builds a client.
func NewClient(opts ClientOptions) (*Client, error) {
	base := NormalizeURL(opts.URL)
	if base == "" {
		return nil, fmt.Errorf("mattermost: no instance url")
	}
	if u, err := url.Parse(base); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("mattermost: %q is not an instance url", opts.URL)
	}
	transport := opts.HTTP
	if transport == nil {
		transport = httpx.Client(DefaultTimeout)
	}
	c := &Client{base: base, token: opts.Token, http: refusingRedirects(transport), now: opts.Now}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// refusingRedirects is hc answering a redirect to its caller rather than
// following it (see "A redirect is refused" above). A copy, so a client the
// caller handed in keeps its own policy everywhere else; the transport, and
// with it the connection pool, is shared.
func refusingRedirects(hc *http.Client) *http.Client {
	out := *hc
	out.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &out
}

// URL is the normalised instance address.
func (c *Client) URL() string { return c.base }

// WebsocketURL is this instance's socket endpoint.
func (c *Client) WebsocketURL() string { return WebsocketURL(c.base) }

// Token is the session this client authenticates with.
func (c *Client) Token() string { return c.token }

// HTTP is the transport this client uses.
//
// Exported so a client built FROM another one — the doctor probing as a
// seat, say — shares its transport rather than opening a second connection
// pool against the same instance.
func (c *Client) HTTP() *http.Client { return c.http }

// request is every call's single path in and out.
//
// repeatable says the caller has established that repeating this request
// cannot repeat an effect — the only thing that makes a non-idempotent
// method retryable on a failure that does not itself prove nothing happened.
func (c *Client) request(ctx context.Context, method, path string, body, out any, repeatable bool) (http.Header, error) {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("mattermost: encode %s %s: %w", method, path, err)
		}
	}

	deadline := c.now().Add(RetryBudget)
	backoff := 200 * time.Millisecond
	for attempt := 0; ; attempt++ {
		header, err := c.attempt(ctx, method, path, encoded, out)
		if err == nil {
			return header, nil
		}
		if !c.mayRetry(err, method, repeatable) || !c.now().Before(deadline) {
			return nil, err
		}
		wait := backoff
		if after := retryAfter(err); after > 0 {
			// The server said how long. Honouring it is the
			// difference between waiting out a rate limit and
			// extending it.
			wait = after
		}
		if remaining := time.Until(deadline); wait > remaining {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		backoff *= 2
	}
}

func (c *Client) attempt(ctx context.Context, method, path string, body []byte, out any) (http.Header, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+APIPath+path, reader)
	if err != nil {
		return nil, fmt.Errorf("mattermost: %s %s: %w", method, path, err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &transportError{method: method, path: path, err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// The server's own error text: it is written for a person, and
		// surfacing it turns "500 on /users/me" into "Invalid session".
		// Read to [httpx.RefusalBytes] and one byte more, so a body past
		// the read is said to be rather than parsed as a shorter one.
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, httpx.RefusalBytes+1))
		return resp.Header, &Error{
			Method: method, Path: path, Status: resp.StatusCode,
			Message:    serverMessage(resp.Header.Get("Content-Type"), payload),
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if resp.StatusCode >= 300 {
		// Drained so the connection returns to the pool.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return resp.Header, &Error{
			Method: method, Path: path, Status: resp.StatusCode,
			Message: redirected(c.base, req.URL, resp.Header.Get("Location")),
		}
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, httpx.MaxResponseBody)).Decode(out); err != nil {
			if media := mediaType(resp.Header.Get("Content-Type")); media != "" && !isJSON(media) {
				return resp.Header, fmt.Errorf("mattermost: %s %s: %s answered with %s rather "+
					"than the API's JSON, so integrations.mattermost.url does not reach "+
					"Mattermost's API. Mattermost answers its web app's page at any path it "+
					"has no route for, which is where an address under another path than "+
					"its Site URL's lands: give the url the Site URL's path: %w",
					method, path, c.base, media, err)
			}
			return resp.Header, fmt.Errorf("mattermost: decode %s %s: %w", method, path, err)
		}
	} else {
		// Drained so the connection returns to the pool rather than
		// being closed and re-dialled on the next call.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	}
	return resp.Header, nil
}

// mayRetry answers the side-effect question, not the status-code one.
func (c *Client) mayRetry(err error, method string, repeatable bool) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	safeForAnyMethod := provesNothingHappened(err)
	if !safeForAnyMethod && !retryable(err) {
		return false
	}
	if safeForAnyMethod || repeatable {
		return true
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete:
		// Idempotent by HTTP's definition and used that way here:
		// repeating one cannot create a second anything.
		return true
	}
	return false
}

// provesNothingHappened reports whether a failure establishes that the
// server never processed the request — the only thing that makes retrying a
// credential-minting POST safe.
func provesNothingHappened(err error) bool {
	if status := Status(err); status != 0 {
		return slices.Contains(rejectedBeforeProcessing, status)
	}
	var t *transportError
	if !errors.As(err, &t) {
		return false
	}
	// A DIAL failure is the transport equivalent: no byte of the request
	// left this process. A read timeout or a reset mid-response is not —
	// there the request may have been applied and only the answer lost,
	// which is the case that mints a second token.
	var op *net.OpError
	if errors.As(t.err, &op) && op.Op == "dial" {
		return true
	}
	return errors.Is(t.err, syscall.ECONNREFUSED)
}

func retryable(err error) bool {
	if status := Status(err); status != 0 {
		return slices.Contains(RetryStatuses, status)
	}
	var t *transportError
	return errors.As(err, &t)
}

// retryAfter reads the server's own Retry-After, in seconds.
func retryAfter(err error) time.Duration {
	var e *Error
	if !errors.As(err, &e) || e.retryAfter <= 0 {
		return 0
	}
	return e.retryAfter
}

// parseRetryAfter reads the header's seconds form. Mattermost sends
// seconds; the HTTP-date form is accepted by the spec and not sent here, and
// guessing at one would be a second clock to disagree with the server's.
func parseRetryAfter(raw string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// transportError is a failure before or during transport, kept distinct from
// an API error so the side-effect question can be asked of it.
type transportError struct {
	method, path string
	err          error
}

func (e *transportError) Error() string {
	return fmt.Sprintf("mattermost: %s %s: %v", e.method, e.path, e.err)
}
func (e *transportError) Unwrap() error { return e.err }

// redirected says where the server sent a request, and what to set
// integrations.mattermost.url to instead.
//
// THE ADDRESS IS DERIVED ONLY WHERE THE REDIRECT SAYS IT. Two redirects reach
// an API client: one that moves the scheme or the host and keeps the path —
// a proxy sending http to https — and Mattermost's own, which puts its Site
// URL's path in front of the WHOLE path of any request outside it
// (server/channels/app/server.go, the RootRouter's NotFoundHandler). So a
// target that ends with the path asked for names the address: what precedes
// that path is the server's own, and the url's path, when it carries one,
// was outside it. Anything else is named as it came, rather than guessed at.
func redirected(base string, requested *url.URL, location string) string {
	target, err := requested.Parse(location)
	if location == "" || err != nil {
		return "the server answered with a redirect it gave no usable address " +
			"for, so integrations.mattermost.url is not the address it serves " +
			"its API at. This client follows no redirect, because following " +
			"one resends every write as a GET"
	}
	set := "the address it answers at without one"
	if prefix, ok := strings.CutSuffix(target.Path, requested.Path); ok {
		at := url.URL{Scheme: target.Scheme, Host: target.Host, Path: prefix}
		if prefix == "" {
			// The path was kept, so the url's own path was right.
			at.Path = urlPath(base)
		}
		set = at.String()
	}
	return fmt.Sprintf("the server redirects it to %s, so integrations.mattermost.url "+
		"is not the address it serves its API at. This client follows no "+
		"redirect, because following one resends every write as a GET: set the "+
		"url to %s", target.Redacted(), set)
}

// mediaType is a Content-Type without its parameters, lower-cased; empty when
// the header is absent or unreadable.
func mediaType(contentType string) string {
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return media
}

// isJSON reports a JSON media type, application/json or a +json suffix.
func isJSON(media string) bool {
	return media == "application/json" || strings.HasSuffix(media, "+json")
}

// serverMessage pulls Mattermost's own error text out of a body, and hands
// anything that is not its envelope to [httpx.Refusal] — a proxy in front of
// a dead server answers with an HTML page, whose title says why, and a body
// past the read is marked rather than reported as no message at all.
func serverMessage(contentType string, payload []byte) string {
	var body struct {
		Message string `json:"message"`
		ID      string `json:"id"`
	}
	if err := json.Unmarshal(payload, &body); err == nil {
		if body.Message != "" {
			return body.Message
		}
		if body.ID != "" {
			return body.ID
		}
	}
	return httpx.Refusal(contentType, payload)
}
