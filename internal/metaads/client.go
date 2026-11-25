package metaads

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// BaseURL is the Graph API's address; the version is a path segment.
const BaseURL = "https://graph.facebook.com"

// requestTimeout bounds one request: a page of 500 rows answers in seconds,
// and an async insights report is polled, never waited on in one request.
const requestTimeout = 60 * time.Second

// maxBody bounds one answer read into memory: a page of Meta's largest list
// (500 rows of a few KiB) stays far below it.
const maxBody = 16 << 20

// reservePercent is the share of a business use case's hourly limit agents'
// reads may spend: the last tenth is kept for approved acts and the pass, so
// an approved pause never fails because agents read insights all hour.
const reservePercent = 90

// usageLife is how long one answer's usage reading stands: Meta reports it on
// every answer, and five minutes of a rolling hour moves it a few percent.
const usageLife = 5 * time.Minute

// Client calls the Graph API for one Meta app.
type Client struct {
	base   string
	http   *http.Client
	secret string
	limit  httpx.RateLimit
	now    func() time.Time
	// deprecated hears Meta saying the pinned version is deprecated.
	deprecated func()

	mu    sync.Mutex
	usage map[string]usage
	tier  string
	// secrets are the values this client calls with, masked wherever Meta's
	// words or an answer could repeat one.
	secrets []string
}

// usage is one business object's busiest limit as Meta last answered it.
type usage struct {
	percent int
	regain  time.Time
	read    time.Time
}

// NewClient is a client for the app whose secret proves every call. A nil hc
// is a guarded client of this package's timeout.
func NewClient(base, secret string, hc *http.Client) *Client {
	if hc == nil {
		hc = httpx.Client(requestTimeout)
	}
	return &Client{base: strings.TrimRight(base, "/"), http: hc, secret: secret, limit: httpx.VendorRateLimit(),
		now: time.Now, usage: map[string]usage{}}
}

// Request is one call. Params are the call's parameters as Meta reads them:
// a GET's, HEAD's or DELETE's go in its query, a write's in its form body.
type Request struct {
	Method, Path string
	Params       url.Values
	// Object is the business object Meta counts the call against (an ad
	// account's digits), "" for none.
	Object string
	// Read marks a call that changes nothing: it alone is retried and held
	// back at the reserve.
	Read bool
	// Reserved is a call of an approved act or the pass, which may spend the
	// reserve.
	Reserved bool
	// NoProof sends no appsecret_proof: the pass's check that Meta refuses a
	// call without one.
	NoProof bool
}

// Response is Meta's answer.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// APIError is Meta's refusal: its error object, redacted and bounded.
type APIError struct {
	Status         int
	Code, Subcode  int
	Type           string
	Message        string
	UserTitle      string
	UserMessage    string
	Transient      bool
	RegainMinutes  int
	MethodAndRoute string
}

// VendorUnavailable reports Meta failing to serve the call: a 5xx or a
// rate limit or an error Meta calls transient.
func (e *APIError) VendorUnavailable() bool {
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests || e.Transient
}

func (e *APIError) Error() string {
	msg := e.Message
	if e.UserMessage != "" {
		msg = e.UserMessage
	}
	return fmt.Sprintf("Meta answered %d (code %d/%d) to %s: %s", e.Status, e.Code, e.Subcode, e.MethodAndRoute, msg)
}

// ErrReserve is a read refused because the business object is down to the
// calls kept for approved acts.
var ErrReserve = errors.New("metaads: the ad account is down to the calls kept for approved acts")

// ErrQuota is a call refused because Meta asked for time to regain access.
var ErrQuota = errors.New("metaads: Meta asked for time to regain access")

// detailBytes bounds Meta's words in an error.
const detailBytes = 600

// throttleCodes are Meta's rate limit errors: the app's, the API score's, the
// account's and the business use cases'.
var throttleCodes = []int{4, 17, 32, 613, 80000, 80001, 80002, 80003, 80004, 80005, 80006, 80008, 80009, 80014}

// Throttled reports whether e is a rate limit.
func (e *APIError) Throttled() bool { return slices.Contains(throttleCodes, e.Code) }

// tokenMarker replaces a secret value in any text the client hands on.
const tokenMarker = "[REDACTED:meta-token]"

// hold notes values as secrets to mask, with each one's proof.
func (c *Client) hold(values ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range values {
		for _, s := range []string{v, c.proof(v)} {
			if s != "" && !slices.Contains(c.secrets, s) {
				c.secrets = append(c.secrets, s)
			}
		}
	}
}

// Mask is text with every secret the client calls with, and every
// credential's shape, redacted: Meta repeats a token in a paging link or an
// error.
func (c *Client) Mask(text string) string {
	c.mu.Lock()
	held := append([]string{c.secret}, c.secrets...)
	c.mu.Unlock()
	for _, s := range held {
		if s != "" {
			text = strings.ReplaceAll(text, s, tokenMarker)
		}
	}
	return redact.Secrets(text)
}

// proof is appsecret_proof for token: HMAC-SHA256 of the token keyed by the
// app secret, in lowercase hex.
func (c *Client) proof(token string) string {
	m := hmac.New(sha256.New, []byte(c.secret))
	m.Write([]byte(token))
	return hex.EncodeToString(m.Sum(nil))
}

// Do makes req with token, which travels in the Authorization header alone.
func (c *Client) Do(ctx context.Context, token string, req Request) (Response, error) {
	c.hold(token)
	if !req.Reserved {
		if err := c.heldBack(req); err != nil {
			return Response{}, fmt.Errorf("%w: %w", errUnsent, err)
		}
	}
	params := url.Values{}
	for k, v := range req.Params {
		params[k] = slices.Clone(v)
	}
	if !req.NoProof {
		params.Set("appsecret_proof", c.proof(token))
	}
	for attempt := 0; ; attempt++ {
		resp, err := c.send(ctx, token, req, params)
		if err != nil {
			return Response{}, err
		}
		c.note(req.Object, resp.Header)
		if resp.Status < 400 {
			return resp, nil
		}
		apiErr := apiError(req, resp, c.Mask)
		if apiErr.Code == 2635 {
			c.heardDeprecated()
		}
		if req.Read && apiErr.Throttled() {
			wait := time.Duration(apiErr.RegainMinutes) * time.Minute
			if wait <= 0 {
				wait = httpx.DefaultRetryAfter
			}
			if c.limit.WaitOut(ctx, attempt, wait) == nil {
				continue
			}
		}
		return resp, apiErr
	}
}

// errUnsent marks a request that failed before it left for Meta: held back
// at the reserve or while Meta asks for time, its request unbuilt or its
// context ended first. Nothing reached Meta.
var errUnsent = errors.New("metaads: the request was not sent to Meta")

// heldBack refuses a read held back at the reserve, and any call while Meta
// asks for time to regain access.
func (c *Client) heldBack(req Request) error {
	if req.Object == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.usage[req.Object]
	now := c.now()
	switch {
	case !ok:
		return nil
	case now.Before(u.regain):
		return fmt.Errorf("%w for about %d minutes", ErrQuota, int(u.regain.Sub(now).Minutes())+1)
	case req.Read && now.Sub(u.read) < usageLife && u.percent >= reservePercent:
		return ErrReserve
	}
	return nil
}

func (c *Client) heardDeprecated() {
	if c.deprecated != nil {
		c.deprecated()
	}
}

// send makes one request. No error it answers names the URL's query, where a
// GET carries appsecret_proof, nor a header.
func (c *Client) send(ctx context.Context, token string, req Request, params url.Values) (Response, error) {
	u := c.base + "/" + Version + req.Path
	var body io.Reader
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodDelete:
		if len(params) > 0 {
			u += "?" + params.Encode()
		}
	default:
		body = strings.NewReader(params.Encode())
	}
	hreq, err := http.NewRequestWithContext(ctx, req.Method, u, body)
	if err != nil {
		return Response{}, fmt.Errorf("metaads: build %s %s: %w: %w", req.Method, req.Path, errUnsent, unwrapURL(err))
	}
	hreq.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		hreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	// A CONTEXT ENDED BEFORE THE SEND sent nothing; one that ends during it
	// may have, so only this side of Do is unsent.
	if ended := ctx.Err(); ended != nil {
		return Response{}, fmt.Errorf("metaads: %s %s: %w: %w", req.Method, req.Path, errUnsent, ended)
	}
	hresp, err := c.http.Do(hreq)
	if err != nil {
		return Response{}, fmt.Errorf("metaads: %s %s: %w", req.Method, req.Path, unwrapURL(err))
	}
	defer func() { _ = hresp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(hresp.Body, maxBody+1))
	if err != nil {
		return Response{}, fmt.Errorf("metaads: read the answer to %s %s: %w", req.Method, req.Path, unwrapURL(err))
	}
	if len(data) > maxBody {
		return Response{}, fmt.Errorf("metaads: the answer to %s %s is over %d MiB", req.Method, req.Path, maxBody>>20)
	}
	if hresp.Header.Get("X-Ad-Api-Version-Warning") != "" {
		c.heardDeprecated()
	}
	return Response{Status: hresp.StatusCode, Header: hresp.Header, Body: data}, nil
}

// unwrapURL is err without the *url.Error around it, whose text is the whole
// URL, query and all.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// apiError reads Meta's error object.
func apiError(req Request, resp Response, mask func(string) string) *APIError {
	var doc struct {
		Error struct {
			Message      string `json:"message"`
			Type         string `json:"type"`
			Code         int    `json:"code"`
			Subcode      int    `json:"error_subcode"`
			UserTitle    string `json:"error_user_title"`
			UserMessage  string `json:"error_user_msg"`
			IsTransient  bool   `json:"is_transient"`
			ErrorData    any    `json:"error_data"`
			RegainAccess int    `json:"estimated_time_to_regain_access"`
		} `json:"error"`
	}
	e := &APIError{Status: resp.Status, MethodAndRoute: req.Method + " " + req.Path}
	if json.Unmarshal(resp.Body, &doc) == nil {
		d := doc.Error
		e.Code, e.Subcode, e.Type, e.Transient = d.Code, d.Subcode, d.Type, d.IsTransient
		e.Message = textcut.Within(mask(d.Message), detailBytes)
		e.UserTitle = textcut.Within(mask(d.UserTitle), detailBytes)
		e.UserMessage = textcut.Within(mask(d.UserMessage), detailBytes)
		e.RegainMinutes = d.RegainAccess
	}
	if e.RegainMinutes == 0 {
		e.RegainMinutes = regainOf(resp.Header, req.Object)
	}
	return e
}

// bucEntry is one business use case's usage in X-Business-Use-Case-Usage.
type bucEntry struct {
	Type         string  `json:"type"`
	CallCount    float64 `json:"call_count"`
	TotalCPUTime float64 `json:"total_cputime"`
	TotalTime    float64 `json:"total_time"`
	Regain       float64 `json:"estimated_time_to_regain_access"`
	Tier         string  `json:"ads_api_access_tier"`
}

// regainOf is the minutes Meta asks object's busiest use case to wait, 0 for none.
func regainOf(h http.Header, object string) int {
	var buc map[string][]bucEntry
	if json.Unmarshal([]byte(h.Get("X-Business-Use-Case-Usage")), &buc) != nil {
		return 0
	}
	most := 0
	for _, e := range buc[object] {
		most = max(most, int(e.Regain))
	}
	return most
}

// note keeps what an answer's usage headers say: each business object's
// busiest limit (Meta's percentages, the minutes to regain access, in minutes
// as its rate limiting page says) and the app's access tier.
func (c *Client) note(object string, h http.Header) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	var buc map[string][]bucEntry
	if json.Unmarshal([]byte(h.Get("X-Business-Use-Case-Usage")), &buc) == nil {
		for id, entries := range buc {
			u := usage{read: now}
			for _, e := range entries {
				u.percent = max(u.percent, int(e.CallCount), int(e.TotalCPUTime), int(e.TotalTime))
				if e.Regain > 0 {
					u.regain = later(u.regain, now.Add(time.Duration(e.Regain)*time.Minute))
				}
				if e.Tier != "" {
					c.tier = e.Tier
				}
			}
			c.usage[id] = u
		}
	}
	var acct struct {
		Util float64 `json:"acc_id_util_pct"`
		Tier string  `json:"ads_api_access_tier"`
	}
	if object != "" && json.Unmarshal([]byte(h.Get("X-Ad-Account-Usage")), &acct) == nil {
		u := c.usage[object]
		u.percent, u.read = max(u.percent, int(acct.Util)), now
		c.usage[object] = u
		if acct.Tier != "" {
			c.tier = acct.Tier
		}
	}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Tier is the app's Marketing API access tier as Meta last answered it, ""
// for none heard.
func (c *Client) Tier() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tier
}

// limitedTiers are the tier names Meta writes for Limited access.
var limitedTiers = []string{"development_access", "standard_access", "limited_access"}

// Limited reports whether tier is Meta's Limited access.
func Limited(tier string) bool { return slices.Contains(limitedTiers, tier) }

// Usage is object's busiest limit in percent as Meta last answered it, -1 for
// none heard within [usageLife].
func (c *Client) Usage(object string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.usage[object]
	if !ok || c.now().Sub(u.read) >= usageLife {
		return -1
	}
	return u.percent
}

// objectOf is the business object Meta counts a call on an ad account
// against: the account's digits.
func objectOf(account string) string { return strings.TrimPrefix(account, "act_") }

// formValue is a parameter's value as Meta reads it: text as written, any
// other value as its JSON.
func formValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	data, _ := json.Marshal(v)
	return string(data)
}
