package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/textcut"
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
	//
	// Where it sent something else — a proxy's HTML page titled or not, a
	// load balancer's sentence, or more than this build will read — it
	// carries a line saying so instead. EMPTY MEANS THE SERVER SENT AN
	// EMPTY BODY and nothing else: [serverMessage] answers Mattermost's own
	// envelope and hands every other outcome to [httpx.RefusalOf], which is
	// the one place that invariant is enforced.
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

	// HTTP is the transport; nil takes one with [DefaultTimeout].
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
	c := &Client{base: base, token: opts.Token, http: opts.HTTP, now: opts.Now}
	if c.http == nil {
		c.http = httpx.Client(DefaultTimeout)
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
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
		// REFUSED PAST THE CEILING, not read up to it. A CAP IS NOT A CUT
		// and this body feeds a DECODER, which is the worst place to put
		// one: io.LimitReader stopped at [httpx.RefusalBytes] and reported
		// io.EOF, so a proxy's HTML page or an oversized envelope arrived
		// as a truncated object, json.Unmarshal failed on it, and the
		// Error carried an EMPTY Message — indistinguishable from a server
		// that genuinely said nothing. [httpx.ReadBody] reads one byte
		// past and refuses, so the overrun becomes something
		// [serverMessage] can state.
		payload, readErr := httpx.ReadBody(resp.Body, httpx.RefusalBytes)
		return resp.Header, &Error{
			Method: method, Path: path, Status: resp.StatusCode,
			Message: serverMessage(
				resp.Header.Get("Content-Type"), payload, readErr),
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, httpx.MaxResponseBody)).Decode(out); err != nil {
			return resp.Header, fmt.Errorf("mattermost: decode %s %s: %w", method, path, err)
		}
	} else {
		// Drained so the connection returns to the pool rather than
		// being closed and re-dialled on the next call.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, httpx.DrainBytes))
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

// serverMessage pulls Mattermost's own error text out of a refusal — and,
// where there is none, says WHICH kind of none it was.
//
// # An empty Message means the server said nothing, and nothing else
//
// It used to mean three different things at once, and only one of them was
// that. The body was read through an io.LimitReader at [httpx.RefusalBytes],
// so a proxy's HTML page or an oversized envelope arrived cut mid-object,
// json.Unmarshal failed on it, and this returned "". A caller holding that
// Error could not tell a server with nothing to say from one that said too
// much to read — which is the first thing an operator needs to know, because
// the two call for opposite next steps.
//
// So every outcome is named now. What is Mattermost's OWN is decided here,
// because its `message` is written for a person and turns "500 on /users/me"
// into "Invalid session". Everything else is [httpx.RefusalOf]'s — a gateway
// page's <title>, a titleless page named as the page it is, a plain sentence
// as itself, and a body that was NOT read reported as exactly that with the
// ceiling that refused it. Only a genuinely empty body yields "", and that
// invariant lives in httpx rather than here.
//
// The vendor SHAPE stays with the vendor and the shared question — "this is
// not a shape I know; what can I say?" — does not, which is the split
// [httpx.Refusal]'s own doc draws. This function is what is left of the
// vendor side of it. [httpx.RefusalOf] is where the rest of the body went
// and why nothing may quote a prefix of it.
func serverMessage(contentType string, payload []byte, err error) string {
	if err == nil {
		var body struct {
			Message string `json:"message"`
			ID      string `json:"id"`
		}
		if json.Unmarshal(payload, &body) == nil {
			// BOUNDED AND MARKED WHERE IT BITES. The read ceiling already
			// bounds this to [httpx.RefusalBytes], but that is five times
			// what an error LINE should carry — [httpx.RefusalDetail] is
			// the tree's named answer for that. [textcut.Within] rather
			// than Ellipsis, because RefusalDetail is a CEILING the suite
			// below asserts, so the marker has to fit inside it; and the
			// marker is what stops a severed sentence reading as a
			// complete one.
			if body.Message != "" {
				return textcut.Within(body.Message, httpx.RefusalDetail)
			}
			if body.ID != "" {
				return textcut.Within(body.ID, httpx.RefusalDetail)
			}
		}
	}
	// EVERY OTHER OUTCOME IS [httpx.RefusalOf]'s.
	// A body that was not read is named with the ceiling that refused it; a
	// proxy's HTML page becomes its <title>, and a titleless one becomes a
	// line saying a page arrived rather than "" — which is what keeps the
	// field doc on [Error.Message] true. The only thing left here is the
	// shape that IS Mattermost's own, above.
	return httpx.RefusalOf(contentType, payload, err)
}
