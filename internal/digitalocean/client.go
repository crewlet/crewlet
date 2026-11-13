package digitalocean

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
)

// APIBase is DigitalOcean's public API, and RevokeURL its OAuth server's
// revoke endpoint, which also revokes a personal access token. Both fixed, so
// a seat's token is only ever sent to DigitalOcean.
const (
	APIBase   = "https://api.digitalocean.com"
	RevokeURL = "https://cloud.digitalocean.com/v1/oauth/revoke"
)

// ClientTimeout bounds one call. Every call runs inside a pass or a paste with
// its own deadline, so this is slack for a slow edge rather than a budget.
const ClientTimeout = 15 * time.Second

// Rate limiting: DigitalOcean allows 250 requests a minute and 5,000 an hour
// per token, and answers 429, which every call waits out by
// [httpx.VendorRateLimit]: a short wait is taken in the call, and a longer one
// is returned to the caller.

// maxResponse bounds what one answer may be: a one-row listing or the account
// is a few kilobytes, and the cap stops a proxy's page being read whole.
const maxResponse = 1 << 20

// APIError is a call DigitalOcean refused with a status of 300 or more. It
// never carries a credential.
type APIError struct {
	Method string
	Path   string
	Status int
	// ID is DigitalOcean's error id ("unauthorized", "Forbidden").
	ID     string
	Detail string

	// RetryAfter is DigitalOcean's own wait on a 429, zero otherwise.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("digitalocean: %s %s: %d: %s", e.Method, e.Path, e.Status, e.Detail)
}

// Status reports the HTTP status a call was refused with, or 0 when the
// failure was not an API error, so [integration.Reject] can classify it.
func Status(err error) int {
	if api, ok := errors.AsType[*APIError](err); ok {
		return api.Status
	}
	return 0
}

// reject is [integration.Reject] for this client's errors.
func reject(err error) error { return integration.Reject(err, Status(err)) }

// Client talks to DigitalOcean with one token.
type Client struct {
	token string
	http  *http.Client
	limit httpx.RateLimit
}

// ClientOptions builds a client.
type ClientOptions struct {
	// Token is the personal access token every call carries.
	Token string
	// HTTP overrides the transport, for tests.
	HTTP *http.Client
}

// NewClient builds a client, refusing an empty token, which would fail every
// call with a misleading 401.
func NewClient(opts ClientOptions) (*Client, error) {
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		return nil, errors.New("digitalocean: no token")
	}
	hc := opts.HTTP
	if hc == nil {
		hc = httpx.Client(ClientTimeout)
	}
	return &Client{token: token, http: hc, limit: httpx.VendorRateLimit()}, nil
}

// Account is the token's user and team, as GET /v2/account answers them.
type Account struct {
	Name   string `json:"name"`
	Email  string `json:"email"`
	UUID   string `json:"uuid"`
	Status string `json:"status"`
	Team   struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"team"`
}

// Locked reports an account DigitalOcean has locked.
func (a Account) Locked() bool { return a.Status == "locked" }

// Account reads the token's user and team, which needs account:read.
func (c *Client) Account(ctx context.Context) (Account, error) {
	var out struct {
		Account Account `json:"account"`
	}
	if _, err := c.call(ctx, http.MethodGet, APIBase+"/v2/account", "", "", &out); err != nil {
		return Account{}, reject(err)
	}
	return out.Account, nil
}

// Answer sends one check request and answers its status, reading nothing
// from the body. Only a 429 DigitalOcean asks to wait out, or the transport
// failing, is an error: every other status is the check's answer.
func (c *Client) Answer(ctx context.Context, method, path, body string) (int, error) {
	status, err := c.call(ctx, method, APIBase+path, body, "application/json", nil)
	if api, ok := errors.AsType[*APIError](err); ok && api.Status != http.StatusTooManyRequests {
		return api.Status, nil
	}
	return status, err
}

// Revoke revokes the client's own token through DigitalOcean's OAuth revoke
// endpoint. A token that already answers 401 is revoked already.
func (c *Client) Revoke(ctx context.Context) error {
	form := url.Values{"token": {c.token}}.Encode()
	_, err := c.call(ctx, http.MethodPost, RevokeURL, form, "application/x-www-form-urlencoded", nil)
	if Status(err) == http.StatusUnauthorized {
		return nil
	}
	return reject(err)
}

// call makes one request and decodes its body into out where out is non-nil,
// waiting out a short 429 as DigitalOcean asks. It is the only place a
// request is built.
func (c *Client) call(ctx context.Context, method, address, body, contentType string, out any) (int, error) {
	return c.callWithin(ctx, method, address, body, contentType, maxResponse, out)
}

// callWithin is [Client.call] reading an answer of at most limit bytes.
func (c *Client) callWithin(
	ctx context.Context, method, address, body, contentType string, limit int64, out any,
) (int, error) {
	for attempt := 0; ; attempt++ {
		status, err := c.once(ctx, method, address, body, contentType, limit, out)
		api, ok := errors.AsType[*APIError](err)
		if ok && api.Status == http.StatusTooManyRequests &&
			c.limit.WaitOut(ctx, attempt, api.RetryAfter) == nil {
			continue
		}
		return status, err
	}
}

// once sends one request and reads its answer.
func (c *Client) once(
	ctx context.Context, method, address, body, contentType string, limit int64, out any,
) (int, error) {
	path := address
	if u, err := url.Parse(address); err == nil {
		path = u.Path
	}
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return 0, fmt.Errorf("digitalocean: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("digitalocean: %s %s: %w", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return res.StatusCode, fmt.Errorf("digitalocean: read %s %s: %w", method, path, err)
	}

	if res.StatusCode >= http.StatusMultipleChoices {
		api := &APIError{Method: method, Path: path, Status: res.StatusCode}
		var refusal struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		}
		if json.Unmarshal(payload, &refusal) == nil {
			api.ID = refusal.ID
			api.Detail = httpx.Refusal("text/plain", []byte(refusal.Message))
		}
		if api.Detail == "" {
			api.Detail = httpx.Refusal(res.Header.Get("Content-Type"), payload)
		}
		if api.Detail == "" {
			api.Detail = "no detail"
		}
		if res.StatusCode == http.StatusTooManyRequests {
			api.RetryAfter = retryAfter(res.Header)
		}
		return res.StatusCode, api
	}
	if out != nil {
		if int64(len(payload)) > limit {
			// CUT SHORT IS NOT AN ANSWER: half a listing is not a short one.
			return res.StatusCode, fmt.Errorf("digitalocean: %s %s answered more than %d bytes", method, path, limit)
		}
		if err := json.Unmarshal(payload, out); err != nil {
			return res.StatusCode, fmt.Errorf("digitalocean: decode %s %s: %w", method, path, err)
		}
	}
	return res.StatusCode, nil
}

// retryAfter reads DigitalOcean's wait: Retry-After where it sends one, else
// ratelimit-reset (a Unix time), else the default.
func retryAfter(h http.Header) time.Duration {
	if wait, ok := httpx.RetryAfter(h, time.Now()); ok {
		return wait
	}
	if reset, err := strconv.ParseInt(strings.TrimSpace(h.Get("Ratelimit-Reset")), 10, 64); err == nil {
		if wait := time.Until(time.Unix(reset, 0)); wait > 0 {
			return wait
		}
	}
	return httpx.DefaultRetryAfter
}
