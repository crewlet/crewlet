package appstoreconnect

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/es256"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// BaseURL is App Store Connect API's address.
const BaseURL = "https://api.appstoreconnect.apple.com"

// The token's timing. Apple refuses a token living past twenty minutes; ten
// minutes of life, starting a minute back, passes a node clock up to nine
// minutes behind Apple's or a minute ahead.
const (
	tokenBackdate = time.Minute
	tokenLife     = 10 * time.Minute
	// tokenRenew is how early a cached token is replaced, so none expires in
	// flight.
	tokenRenew = time.Minute
)

// maxBody bounds one answer read into memory: a page of 200 resources with
// their included relations stays far below it; a report streams instead.
const maxBody = 16 << 20

// reserveShare is the share of a key's hourly limit kept for approved acts and
// the pass: an approved reply must not fail because agents read every review
// that hour.
const reserveShare = 10

// Key is one team key the client signs with.
type Key struct {
	ID  string
	Key *ecdsa.PrivateKey
}

// Client calls App Store Connect for one team.
type Client struct {
	base   string
	http   *http.Client
	issuer string
	keys   map[string]Key
	limit  httpx.RateLimit
	now    func() time.Time

	mu     sync.Mutex
	tokens map[string]cachedToken
	rates  map[string]rate

	// answered hears each answer's status and Apple's clock, for the
	// clock-skew notice; nil hears nothing.
	answered func(status int, date time.Time)
}

type cachedToken struct {
	token string
	until time.Time
}

// rate is a key's hourly budget as Apple last answered it.
type rate struct {
	limit, remaining int
	read             time.Time
}

// NewClient is a client for the team issuer, signing with keys by slot. A nil
// hc is a guarded client of this package's timeout.
func NewClient(base, issuer string, keys map[string]Key, hc *http.Client) *Client {
	if hc == nil {
		hc = httpx.Client(requestTimeout)
	}
	return &Client{base: strings.TrimRight(base, "/"), http: hc, issuer: issuer, keys: keys,
		limit: httpx.VendorRateLimit(), now: time.Now, tokens: map[string]cachedToken{}, rates: map[string]rate{}}
}

// requestTimeout bounds one request: a page of 200 reviews answers in about a
// second; a report download streams under the caller's deadline instead.
const requestTimeout = 60 * time.Second

// Request is one call: Read marks a call that changes nothing, which alone is
// retried after a 429 and alone is held back by the reserve.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Body   any
	Read   bool
	// Reserved is a call of an approved act or the pass, which may spend the
	// reserve.
	Reserved bool
	// Accept is the answer's media type; empty is JSON.
	Accept string
}

// Response is Apple's answer.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// APIError is Apple's refusal: its status and its errors' first code, title
// and detail, redacted and bounded.
type APIError struct {
	Status int
	Code   string
	Title  string
	Detail string
	// Date is Apple's clock when it refused, for the clock-skew notice.
	Date time.Time
}

// VendorUnavailable reports App Store Connect failing to serve the call: a 5xx or a
// rate limit.
func (e *APIError) VendorUnavailable() bool {
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests
}

func (e *APIError) Error() string {
	return fmt.Sprintf("App Store Connect answered %d %s: %s %s", e.Status, e.Code, e.Title, e.Detail)
}

// ErrReserve is a read refused because the key is down to its reserve.
var ErrReserve = errors.New("appstoreconnect: the key is down to the calls kept for approved acts")

// ErrRole is a read the agent's own key's role may not make.
var ErrRole = errors.New("appstoreconnect: the agent's key's role may not make this read")

// errNoSlot is a client asked to sign as a key it does not hold.
var errNoSlot = errors.New("appstoreconnect: the client holds no such key")

// errUnsent marks a request that failed before it left for Apple: a key the
// client does not hold, held at the reserve, its body unencoded or its token
// unsigned, its request unbuilt or its context ended first. Nothing reached
// Apple.
var errUnsent = errors.New("appstoreconnect: the request was not sent to Apple")

// detailBytes bounds Apple's words in an error.
const detailBytes = 600

// Do makes req as slot's key.
func (c *Client) Do(ctx context.Context, slot string, req Request) (Response, error) {
	key, ok := c.keys[slot]
	if !ok {
		return Response{}, fmt.Errorf("%w: %w: %s", errUnsent, errNoSlot, slot)
	}
	if req.Read && !req.Reserved && c.atReserve(slot) {
		return Response{}, fmt.Errorf("%w: %w", errUnsent, ErrReserve)
	}
	var body []byte
	if req.Body != nil {
		b, err := json.Marshal(req.Body)
		if err != nil {
			return Response{}, fmt.Errorf("appstoreconnect: encode the request body: %w: %w", errUnsent, err)
		}
		body = b
	}
	token, err := c.token(slot, key, req.Method, req.Path, req.Query)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %w", errUnsent, err)
	}
	for attempt := 0; ; attempt++ {
		resp, err := c.send(ctx, token, req, body)
		if err != nil {
			return Response{}, err
		}
		c.noteRate(slot, resp.Header)
		if c.answered != nil {
			if date, perr := http.ParseTime(resp.Header.Get("Date")); perr == nil {
				c.answered(resp.Status, date)
			}
		}
		if resp.Status == http.StatusTooManyRequests && req.Read {
			wait, ok := httpx.RetryAfter(resp.Header, c.now())
			if !ok {
				wait = httpx.DefaultRetryAfter
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

func (c *Client) send(ctx context.Context, token string, req Request, body []byte) (Response, error) {
	method, path, query := req.Method, req.Path, req.Query
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return Response{}, fmt.Errorf("appstoreconnect: build the request: %w: %w", errUnsent, err)
	}
	hreq.Header.Set("Authorization", "Bearer "+token)
	accept := req.Accept
	if accept == "" {
		accept = "application/json"
	}
	hreq.Header.Set("Accept", accept)
	if body != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	// A CONTEXT ENDED BEFORE THE SEND sent nothing; one that ends during it
	// may have, so only this side of Do is unsent.
	if ended := ctx.Err(); ended != nil {
		return Response{}, fmt.Errorf("appstoreconnect: %s %s: %w: %w", method, path, errUnsent, ended)
	}
	hresp, err := c.http.Do(hreq)
	if err != nil {
		return Response{}, fmt.Errorf("appstoreconnect: %s %s: %w", method, path, err)
	}
	defer func() { _ = hresp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(hresp.Body, maxBody+1))
	if err != nil {
		return Response{}, fmt.Errorf("appstoreconnect: read the answer to %s %s: %w", method, path, err)
	}
	if len(data) > maxBody {
		return Response{}, fmt.Errorf("appstoreconnect: the answer to %s %s is over %d MiB", method, path, maxBody>>20)
	}
	return Response{Status: hresp.StatusCode, Header: hresp.Header, Body: data}, nil
}

// apiError reads Apple's errors document.
func apiError(resp Response) error {
	var doc struct {
		Errors []struct {
			Code, Title, Detail string
		} `json:"errors"`
	}
	e := &APIError{Status: resp.Status}
	if t, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		e.Date = t
	}
	if json.Unmarshal(resp.Body, &doc) == nil && len(doc.Errors) > 0 {
		first := doc.Errors[0]
		e.Code, e.Title = first.Code, textcut.Within(redact.Secrets(first.Title), detailBytes)
		e.Detail = textcut.Within(redact.Secrets(first.Detail), detailBytes)
	}
	return e
}

// unscopedQuery are the parameters Apple ignores when it checks a token's
// scope.
var unscopedQuery = map[string]bool{"limit": true, "cursor": true, "sort": true}

// Scope is the token scope of one request: the method, the path and every
// query parameter but limit, cursor and sort, in key order.
func Scope(method, path string, query url.Values) string {
	keys := make([]string, 0, len(query))
	for k := range query {
		if !unscopedQuery[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range query[k] {
			parts = append(parts, k+"="+v)
		}
	}
	s := method + " " + path
	if len(parts) > 0 {
		s += "?" + strings.Join(parts, "&")
	}
	return s
}

// token is a token for slot scoped to the request, reused while it lives: a
// token that reaches a log can repeat only the one call it was made for.
func (c *Client) token(slot string, key Key, method, path string, query url.Values) (string, error) {
	scope := Scope(method, path, query)
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	cacheKey := slot + "\x00" + scope
	if t, ok := c.tokens[cacheKey]; ok && now.Before(t.until) {
		return t.token, nil
	}
	iat := now.Add(-tokenBackdate)
	tok, err := es256.Sign(key.Key, key.ID, map[string]any{"iss": c.issuer, "iat": iat.Unix(),
		"exp": iat.Add(tokenLife).Unix(), "aud": "appstoreconnect-v1", "scope": []string{scope}})
	if err != nil {
		return "", fmt.Errorf("appstoreconnect: sign the %s key's token: %w", slot, err)
	}
	if len(c.tokens) >= maxCachedTokens {
		c.tokens = map[string]cachedToken{}
	}
	c.tokens[cacheKey] = cachedToken{token: tok, until: iat.Add(tokenLife - tokenRenew)}
	return tok, nil
}

// maxCachedTokens bounds the token cache: one per distinct request a key made
// in its tokens' life, far past what agents ask in ten minutes.
const maxCachedTokens = 4096

// rateHeader reads `X-Rate-Limit: user-hour-lim:3500;user-hour-rem:500;`.
var rateHeader = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`user-hour-(lim|rem):([0-9]+)`)
})

func (c *Client) noteRate(slot string, h http.Header) {
	raw := h.Get("X-Rate-Limit")
	if raw == "" {
		return
	}
	r := rate{read: c.now()}
	for _, m := range rateHeader().FindAllStringSubmatch(raw, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		if m[1] == "lim" {
			r.limit = n
		} else {
			r.remaining = n
		}
	}
	if r.limit == 0 {
		return
	}
	c.mu.Lock()
	c.rates[slot] = r
	c.mu.Unlock()
}

// atReserve reports whether slot's key is down to its reserve, as Apple last
// answered within the hour.
func (c *Client) atReserve(slot string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.rates[slot]
	if !ok || c.now().Sub(r.read) > time.Hour {
		return false
	}
	return r.remaining*100 <= r.limit*reserveShare
}

// Remaining is slot's last known hourly limit and remaining calls; zero for
// none read within the hour.
func (c *Client) Remaining(slot string) (limit, remaining int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.rates[slot]
	if !ok || c.now().Sub(r.read) > time.Hour {
		return 0, 0
	}
	return r.limit, r.remaining
}
