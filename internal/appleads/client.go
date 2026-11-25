package appleads

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/es256"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// BaseURL is the Apple Ads Platform API's address. The Platform API is the
// only one called: the Campaign Management API v5 shuts down on 2027-01-26.
const BaseURL = "https://api.ads.apple.com"

// TokenURL is where a client secret is exchanged for an access token.
const TokenURL = "https://appleid.apple.com/auth/oauth2/token"

// The client secret's timing. Apple takes a secret living up to 180 days; ten
// minutes from a minute back passes a node clock a minute ahead or nine behind,
// and no long-lived secret exists outside the secret store.
const (
	secretBackdate = time.Minute
	secretLife     = 10 * time.Minute
	// tokenRenew is how early a cached access token is replaced, so none
	// expires in flight.
	tokenRenew = time.Minute
)

// maxBody bounds one answer read into memory: a report page of 5,000 rows
// with their metadata stays below it.
const maxBody = 32 << 20

// reserveShare is the share of a connection's rate window kept for approved
// acts and the pass: an approved raise must not fail because agents read
// every report that minute.
const reserveShare = 10

// maxRetryWait is the longest Retry-After a read waits out: a longer one is
// Apple's quota for the window, answered as such rather than waited on.
const maxRetryWait = 30 * time.Second

// requestTimeout bounds one request: a report page answers in seconds.
const requestTimeout = 60 * time.Second

// Credentials are one API user's client credentials and its private key.
type Credentials struct {
	ClientID, TeamID, KeyID string
	Key                     *ecdsa.PrivateKey
}

// Client calls the Apple Ads Platform API as one API user.
type Client struct {
	base, tokenURL string
	http           *http.Client
	creds          Credentials
	limit          httpx.RateLimit
	now            func() time.Time

	mu    sync.Mutex
	token string
	until time.Time
	rate  rate

	// answered hears whether Apple accepted a freshly signed secret, and its
	// clock, for the clock-skew notice; nil hears nothing.
	answered func(refused bool, date time.Time)
}

// rate is the connection's window as Apple last answered it.
type rate struct {
	limit, remaining int
	resets           time.Time
}

// NewClient is a client for one API user. A nil hc is a guarded client of
// this package's timeout.
func NewClient(base, tokenURL string, creds Credentials, hc *http.Client) *Client {
	if hc == nil {
		hc = httpx.Client(requestTimeout)
	}
	return &Client{base: strings.TrimRight(base, "/"), tokenURL: tokenURL, http: hc, creds: creds,
		limit: httpx.VendorRateLimit(), now: time.Now}
}

// Request is one call. Account sets `X-AP-Context`; "" is an organization
// call. Read marks a call that changes nothing, which alone is retried after
// a 429 and alone is held back by the reserve.
type Request struct {
	Method  string
	Path    string
	Query   url.Values
	Body    any
	Upload  *Upload
	Account string
	Read    bool
	// Reserved is a call of an approved act or the pass, which may spend the
	// reserve.
	Reserved bool
}

// Upload is a file sent as multipart form data (uploadAsset), with its form
// fields.
type Upload struct {
	Name   string
	Data   []byte
	Fields map[string]string
}

// Response is Apple's answer.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// APIError is Apple's refusal: its status, code and message, redacted and
// bounded.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Apple Ads answered %d %s: %s", e.Status, e.Code, e.Message)
}

// ErrCredentials is Apple refusing the API user's client secret: its key was
// removed or replaced, or the user lost its API role.
var ErrCredentials = errors.New("appleads: Apple refused the API user's client credentials")

// ErrReserve is a read refused because the connection is down to its reserve.
var ErrReserve = errors.New("appleads: the connection is down to the calls kept for approved acts")

// ErrQuota is a 429 whose wait is past what a read waits out.
var ErrQuota = errors.New("appleads: Apple asked to slow down for longer than a read waits")

// errUnsent marks a request that failed before it left for Apple: held at
// the reserve, its body unencoded, no access token to send it with, its
// request unbuilt or its context ended first. Nothing reached Apple.
var errUnsent = errors.New("appleads: the request was not sent to Apple")

// detailBytes bounds Apple's words in an error.
const detailBytes = 600

// Do makes req.
func (c *Client) Do(ctx context.Context, req Request) (Response, error) {
	if req.Read && !req.Reserved && c.atReserve() {
		return Response{}, fmt.Errorf("%w: %w", errUnsent, ErrReserve)
	}
	body, contentType, err := encode(req)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %w", errUnsent, err)
	}
	fresh := false
	for attempt := 0; ; attempt++ {
		// A TOKEN THAT COULD NOT BE HAD sent nothing: the first round sent no
		// request, and a later one follows a 401, which made nothing.
		token, minted, err := c.accessToken(ctx)
		if err != nil {
			return Response{}, fmt.Errorf("%w: %w", errUnsent, err)
		}
		fresh = fresh || minted
		resp, err := c.send(ctx, token, req, body, contentType)
		if err != nil {
			return Response{}, err
		}
		c.noteRate(resp.Header)
		switch {
		case resp.Status == http.StatusUnauthorized && !fresh:
			// A CACHED TOKEN APPLE NO LONGER TAKES is replaced once: a 401 made
			// nothing, so a write sent again is still sent once.
			c.forget(token)
			fresh = true
			continue
		case resp.Status == http.StatusTooManyRequests && req.Read:
			wait, ok := httpx.RetryAfter(resp.Header, c.now())
			if !ok {
				wait = httpx.DefaultRetryAfter
			}
			if wait > maxRetryWait {
				return resp, fmt.Errorf("%w: %s", ErrQuota, wait.Round(time.Second))
			}
			if c.limit.WaitOut(ctx, attempt, wait) == nil {
				continue
			}
		}
		if resp.Status >= 400 {
			return resp, apiError(resp)
		}
		return resp, nil
	}
}

// encode is req's body and content type.
func encode(req Request) ([]byte, string, error) {
	if req.Upload != nil {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		for k, v := range req.Upload.Fields {
			if err := w.WriteField(k, v); err != nil {
				return nil, "", fmt.Errorf("appleads: encode the upload: %w", err)
			}
		}
		part, err := w.CreateFormFile("file", req.Upload.Name)
		if err == nil {
			_, err = part.Write(req.Upload.Data)
		}
		if err == nil {
			err = w.Close()
		}
		if err != nil {
			return nil, "", fmt.Errorf("appleads: encode the upload: %w", err)
		}
		return buf.Bytes(), w.FormDataContentType(), nil
	}
	if req.Body == nil {
		return nil, "", nil
	}
	b, err := json.Marshal(req.Body)
	if err != nil {
		return nil, "", fmt.Errorf("appleads: encode the request body: %w", err)
	}
	return b, "application/json", nil
}

func (c *Client) send(ctx context.Context, token string, req Request, body []byte, contentType string) (Response, error) {
	u := c.base + req.Path
	if len(req.Query) > 0 {
		u += "?" + req.Query.Encode()
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	hreq, err := http.NewRequestWithContext(ctx, req.Method, u, r)
	if err != nil {
		return Response{}, fmt.Errorf("appleads: build the request: %w: %w", errUnsent, err)
	}
	hreq.Header.Set("Authorization", "Bearer "+token)
	hreq.Header.Set("Accept", "application/json")
	if req.Account != "" {
		hreq.Header.Set("X-AP-Context", "adAccountId="+req.Account)
	}
	if contentType != "" {
		hreq.Header.Set("Content-Type", contentType)
	}
	// A CONTEXT ENDED BEFORE THE SEND sent nothing; one that ends during it
	// may have, so only this side of Do is unsent.
	if ended := ctx.Err(); ended != nil {
		return Response{}, fmt.Errorf("appleads: %s %s: %w: %w", req.Method, req.Path, errUnsent, ended)
	}
	hresp, err := c.http.Do(hreq)
	if err != nil {
		return Response{}, fmt.Errorf("appleads: %s %s: %w", req.Method, req.Path, err)
	}
	defer func() { _ = hresp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(hresp.Body, maxBody+1))
	if err != nil {
		return Response{}, fmt.Errorf("appleads: read the answer to %s %s: %w", req.Method, req.Path, err)
	}
	if len(data) > maxBody {
		return Response{}, fmt.Errorf("appleads: the answer to %s %s is over %d MiB", req.Method, req.Path, maxBody>>20)
	}
	return Response{Status: hresp.StatusCode, Header: hresp.Header, Body: data}, nil
}

// apiError reads Apple's error, enveloped (`{"error": {...}}`) or bare.
func apiError(resp Response) error {
	var doc struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	e := &APIError{Status: resp.Status}
	if json.Unmarshal(resp.Body, &doc) == nil {
		e.Code, e.Message = doc.Code, doc.Message
		if doc.Error != nil {
			e.Code, e.Message = doc.Error.Code, doc.Error.Message
		}
	}
	e.Code = textcut.Within(redact.Secrets(e.Code), 80)
	e.Message = textcut.Within(redact.Secrets(e.Message), detailBytes)
	return e
}

// accessToken is a live access token, minted is true where it was exchanged
// for this call.
func (c *Client) accessToken(ctx context.Context) (string, bool, error) {
	now := c.now()
	c.mu.Lock()
	if c.token != "" && now.Before(c.until) {
		t := c.token
		c.mu.Unlock()
		return t, false, nil
	}
	c.mu.Unlock()
	iat := now.Add(-secretBackdate)
	secret, err := es256.Sign(c.creds.Key, c.creds.KeyID, map[string]any{"iss": c.creds.TeamID,
		"sub": c.creds.ClientID, "aud": "https://appleid.apple.com", "iat": iat.Unix(),
		"exp": iat.Add(secretLife).Unix()})
	if err != nil {
		return "", false, fmt.Errorf("appleads: sign the client secret: %w", err)
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.creds.ClientID},
		"client_secret": {secret}, "scope": {"searchadsorg"}}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", false, fmt.Errorf("appleads: build the token request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hreq.Header.Set("Accept", "application/json")
	hresp, err := c.http.Do(hreq)
	if err != nil {
		// THE URL CARRIES NO SECRET, but the transport's error is not said:
		// it names the request, and the form is the request's body.
		return "", false, errors.New("appleads: the token endpoint could not be reached")
	}
	defer func() { _ = hresp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(hresp.Body, 64<<10))
	date, _ := http.ParseTime(hresp.Header.Get("Date"))
	var doc struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(data, &doc)
	if hresp.StatusCode == http.StatusBadRequest || hresp.StatusCode == http.StatusUnauthorized {
		if c.answered != nil && !date.IsZero() {
			c.answered(true, date)
		}
		return "", false, fmt.Errorf("%w (%s)", ErrCredentials, textcut.Within(redact.Secrets(doc.Error), 80))
	}
	if hresp.StatusCode != http.StatusOK || doc.AccessToken == "" || doc.ExpiresIn <= 0 {
		return "", false, &APIError{Status: hresp.StatusCode, Code: textcut.Within(redact.Secrets(doc.Error), 80),
			Message: "the token endpoint answered no access token"}
	}
	if c.answered != nil && !date.IsZero() {
		c.answered(false, date)
	}
	until := now.Add(time.Duration(doc.ExpiresIn)*time.Second - tokenRenew)
	c.mu.Lock()
	c.token, c.until = doc.AccessToken, until
	c.mu.Unlock()
	return doc.AccessToken, true, nil
}

// forget drops token, where it is still the cached one.
func (c *Client) forget(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token, c.until = "", time.Time{}
	}
}

// noteRate reads `RateLimit-Limit`, `RateLimit-Remaining` and
// `RateLimit-Reset` (seconds), which Apple sends on every answer.
func (c *Client) noteRate(h http.Header) {
	limit, err1 := strconv.Atoi(strings.TrimSpace(h.Get("RateLimit-Limit")))
	remaining, err2 := strconv.Atoi(strings.TrimSpace(h.Get("RateLimit-Remaining")))
	reset, err3 := strconv.Atoi(strings.TrimSpace(h.Get("RateLimit-Reset")))
	if err1 != nil || err2 != nil || err3 != nil || limit <= 0 || reset < 0 {
		return
	}
	c.mu.Lock()
	c.rate = rate{limit: limit, remaining: remaining, resets: c.now().Add(time.Duration(reset) * time.Second)}
	c.mu.Unlock()
}

// atReserve reports whether the connection is down to its reserve in the
// window Apple last answered.
func (c *Client) atReserve() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rate
	if r.limit == 0 || !c.now().Before(r.resets) {
		return false
	}
	return r.remaining*reserveShare <= r.limit
}

// Remaining is the window's limit, the calls left in it and when it resets;
// zero for none read, or a window that has reset.
func (c *Client) Remaining() (limit, remaining int, resets time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rate.limit == 0 || !c.now().Before(c.rate.resets) {
		return 0, 0, time.Time{}
	}
	return c.rate.limit, c.rate.remaining, c.rate.resets
}
