package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
)

// APIBase is Cloudflare's v4 API. Fixed rather than configurable, so a
// provisioner token is only ever sent to Cloudflare.
const APIBase = "https://api.cloudflare.com/client/v4"

// ClientTimeout bounds one call. Every call runs inside a pass with its own
// deadline, so this is slack for a slow edge rather than a budget to spend.
const ClientTimeout = 15 * time.Second

// Rate limiting: Cloudflare allows 1,200 requests per five minutes per token
// and answers 429 with `retry-after`, which every call waits out by
// [httpx.VendorRateLimit]: a short wait is taken in the call, and a longer one
// is returned to the pass, whose loop backs off on its own schedule.

// maxResponse bounds what one answer may be: a page of 50 zones or tokens is
// tens of kilobytes, and the cap stops a proxy's page being read whole.
const maxResponse = 4 << 20

// accountIDPattern is the shape Cloudflare gives every account identifier.
// Compiled at first use, not at init: Gitlet's push hook is this
// binary, and pays every package's init (cmd/crewlet's hookinit_test.go); the
// bounded repeat compiles to a program holding every count, about 13 KB.
var accountIDPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[0-9a-f]{32}$`)
})

// Message is one entry of an envelope's `errors` or `messages`, with the
// errors Cloudflare chains under it.
type Message struct {
	Code    int       `json:"code"`
	Message string    `json:"message"`
	Chain   []Message `json:"error_chain,omitempty"`
}

// APIError is a call Cloudflare refused: a status of 300 or more, or a 2xx
// whose envelope says `success: false`. It never carries a credential.
type APIError struct {
	Method string
	Path   string
	Status int
	Codes  []int
	Detail string

	// RetryAfter is Cloudflare's own wait on a 429, zero otherwise.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cloudflare: %s %s: %d: %s", e.Method, e.Path, e.Status, e.Detail)
}

// Status reports the HTTP status a call was refused with, or 0 when the
// failure was not an API error, so [integration.Reject] can classify it.
func Status(err error) int {
	if api, ok := errors.AsType[*APIError](err); ok {
		return api.Status
	}
	return 0
}

// tokenRefusedCodes are Cloudflare's error codes for a value that is not a
// token at all: 1000 Invalid API Token, 6003 Invalid request headers and 6111
// Invalid format for Authorization header. Cloudflare answers them with 400,
// which on its fixed API says nothing about the address.
//
//nolint:gochecknoglobals // an immutable set, not state
var tokenRefusedCodes = []int{1000, 6003, 6111}

// TokenRefused reports a refusal of the token itself, by Cloudflare's error
// codes rather than its status.
func TokenRefused(err error) bool {
	api, ok := errors.AsType[*APIError](err)
	return ok && slices.ContainsFunc(api.Codes, func(c int) bool { return slices.Contains(tokenRefusedCodes, c) })
}

// reject is [integration.Reject] with Cloudflare's own word for a refused
// token: a 400 whose codes refuse the token is the credential's, as a 401 is.
func reject(err error) error {
	if TokenRefused(err) && !errors.Is(err, integration.ErrCredentialRejected) {
		return fmt.Errorf("%w: %w", integration.ErrCredentialRejected, err)
	}
	return integration.Reject(err, Status(err))
}

// noDetail is what a refusal that said nothing reads as.
const noDetail = "no detail"

// Client talks to one Cloudflare account with one token.
type Client struct {
	account string
	token   string
	http    *http.Client
	limit   httpx.RateLimit

	// calls counts every request sent, retries included: what a read of
	// the account for the atlas cost at the vendor.
	calls atomic.Int64
}

// ClientOptions builds a client.
type ClientOptions struct {
	// AccountID is the account every account-scoped route addresses.
	AccountID string
	// Token is the API token every call carries.
	Token string
	// HTTP overrides the transport, for tests.
	HTTP *http.Client
}

// NewClient builds a client, refusing an account ID that is not one and an
// empty token, either of which would fail every call with a misleading 4xx.
func NewClient(opts ClientOptions) (*Client, error) {
	account := strings.TrimSpace(opts.AccountID)
	if !accountIDPattern().MatchString(account) {
		return nil, errors.New("cloudflare: the account ID is not 32 lowercase hexadecimal characters")
	}
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		return nil, errors.New("cloudflare: no API token")
	}
	hc := opts.HTTP
	if hc == nil {
		hc = httpx.Client(ClientTimeout)
	}
	return &Client{account: account, token: token, http: hc, limit: httpx.VendorRateLimit()}, nil
}

// AccountID is the account this client addresses.
func (c *Client) AccountID() string { return c.account }

// Calls is how many requests the client has sent, retries after a rate limit
// included: what a read of the account for the atlas cost at the vendor.
func (c *Client) Calls() int { return int(c.calls.Load()) }

// TokensURL is the dashboard page listing this account's API tokens, where a
// person settles what only Cloudflare can.
func (c *Client) TokensURL() string { return TokensURL(c.account) }

// TokensURL is the account API tokens page for one account.
func TokensURL(accountID string) string {
	return "https://dash.cloudflare.com/" + url.PathEscape(accountID) + "/api-tokens"
}

// envelope is Cloudflare's response shape on every route.
type envelope struct {
	Success    bool            `json:"success"`
	Errors     []Message       `json:"errors"`
	Messages   []Message       `json:"messages"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *resultInfo     `json:"result_info"`
}

// resultInfo is a listing's paging, as far as Cloudflare reports it.
type resultInfo struct {
	Page       int `json:"page"`
	PerPage    int `json:"per_page"`
	Count      int `json:"count"`
	TotalCount int `json:"total_count"`
	TotalPages int `json:"total_pages"`

	// Cursor is where the next page starts, on the routes that page by one
	// (R2's buckets); empty on the last page and on every other route.
	Cursor string `json:"cursor"`
}

// do makes one call and decodes its envelope's result into out, waiting out
// a short 429 as Cloudflare asks. It is the only place a request is built.
func (c *Client) do(
	ctx context.Context, method, path string, query url.Values, body, out any,
) (*resultInfo, error) {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("cloudflare: encode %s %s: %w", method, path, err)
		}
	}
	for attempt := 0; ; attempt++ {
		env, err := c.once(ctx, method, path, query, encoded)
		var api *APIError
		if errors.As(err, &api) && api.Status == http.StatusTooManyRequests &&
			c.limit.WaitOut(ctx, attempt, api.RetryAfter) == nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		if out != nil && len(env.Result) > 0 {
			if err := json.Unmarshal(env.Result, out); err != nil {
				return nil, fmt.Errorf("cloudflare: decode %s %s: %w", method, path, err)
			}
		}
		return env.ResultInfo, nil
	}
}

// once sends one request and reads its envelope.
func (c *Client) once(
	ctx context.Context, method, path string, query url.Values, body []byte,
) (envelope, error) {
	address := APIBase + path
	if len(query) > 0 {
		address += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return envelope{}, fmt.Errorf("cloudflare: build %s %s: %w", method, path, err)
	}
	c.calls.Add(1)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return envelope{}, fmt.Errorf("cloudflare: %s %s: %w", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return envelope{}, fmt.Errorf("cloudflare: read %s %s: %w", method, path, err)
	}
	var env envelope
	decodeErr := json.Unmarshal(payload, &env)
	if res.StatusCode >= http.StatusMultipleChoices || (decodeErr == nil && !env.Success) {
		api := &APIError{Method: method, Path: path, Status: res.StatusCode}
		if decodeErr == nil {
			api.Codes, api.Detail = refusalOf(env.Errors)
		}
		if api.Detail == "" {
			api.Detail = httpx.Refusal(res.Header.Get("Content-Type"), payload)
		}
		if api.Detail == "" {
			api.Detail = noDetail
		}
		if res.StatusCode == http.StatusTooManyRequests {
			api.RetryAfter = retryAfter(res.Header)
		}
		return envelope{}, api
	}
	if decodeErr != nil {
		return envelope{}, fmt.Errorf("cloudflare: decode %s %s: %w", method, path, decodeErr)
	}
	return env, nil
}

// refusalOf is an envelope's error codes, its chained ones included, and its
// messages as one bounded sentence.
func refusalOf(errs []Message) ([]int, string) {
	var codes []int
	var texts []string
	var walk func([]Message)
	walk = func(errs []Message) {
		for _, e := range errs {
			codes = append(codes, e.Code)
			if text := strings.TrimSpace(e.Message); text != "" {
				texts = append(texts, text)
			}
			walk(e.Chain)
		}
	}
	walk(errs)
	if len(texts) == 0 {
		return codes, ""
	}
	return codes, httpx.Refusal("text/plain", []byte(strings.Join(texts, "; ")))
}

// retryAfter reads Cloudflare's wait from `retry-after`, or the default where
// it named none.
func retryAfter(h http.Header) time.Duration {
	if wait, ok := httpx.RetryAfter(h, time.Now()); ok {
		return wait
	}
	return httpx.DefaultRetryAfter
}

// Paging: listings ask for listPageSize rows, the largest Cloudflare serves on
// the zone listing, and stop after listCeiling pages (10,000 rows) with an
// error, since a short read would read as rows that do not exist.
const (
	listPageSize = 50
	listCeiling  = 200
)

// list reads a paged collection to exhaustion, handing each page's rows to
// add, which answers how many it held.
func (c *Client) list(
	ctx context.Context, path string, query url.Values, add func(json.RawMessage) (int, error),
) error {
	return c.listPages(ctx, path, query, listPageSize, add)
}

// listPages is [Client.list] at a page size of the route's own: the DNS
// records of a zone serve far more rows a page than the zone listing does.
func (c *Client) listPages(
	ctx context.Context, path string, query url.Values, perPage int, add func(json.RawMessage) (int, error),
) error {
	collected := 0
	for page := 1; ; page++ {
		if page > listCeiling {
			return fmt.Errorf("cloudflare: %s did not finish after %d pages of %d", path, listCeiling, perPage)
		}
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", strconv.Itoa(perPage))
		var rows json.RawMessage
		info, err := c.do(ctx, http.MethodGet, path, q, nil, &rows)
		if err != nil {
			return err
		}
		n, err := add(rows)
		if err != nil {
			return fmt.Errorf("cloudflare: decode %s: %w", path, err)
		}
		collected += n
		if lastPage(info, page, n, collected, perPage) {
			return nil
		}
	}
}

// lastPage reports a listing that is complete: no paging at all, the last
// page Cloudflare counts, every row it counts, or a short or empty page.
func lastPage(info *resultInfo, page, got, collected, perPage int) bool {
	switch {
	case info == nil || got == 0:
		return true
	case info.TotalPages > 0:
		return page >= info.TotalPages
	case info.TotalCount > 0:
		return collected >= info.TotalCount
	default:
		return got < perPage
	}
}

// accountPath is an account-scoped route.
func (c *Client) accountPath(rest string) string {
	return "/accounts/" + url.PathEscape(c.account) + rest
}

// TokenStatus is what Cloudflare says about the token a client carries.
type TokenStatus struct {
	ID        string
	Status    string
	ExpiresOn time.Time
	NotBefore time.Time
}

// The statuses Cloudflare gives a token.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
	StatusExpired  = "expired"
)

// Verify reads the status of the client's own token as an account token.
func (c *Client) Verify(ctx context.Context) (TokenStatus, error) {
	var body struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		ExpiresOn string `json:"expires_on"`
		NotBefore string `json:"not_before"`
	}
	if _, err := c.do(ctx, http.MethodGet, c.accountPath("/tokens/verify"), nil, nil, &body); err != nil {
		return TokenStatus{}, err
	}
	return TokenStatus{
		ID: body.ID, Status: body.Status,
		ExpiresOn: parseTime(body.ExpiresOn), NotBefore: parseTime(body.NotBefore),
	}, nil
}

// parseTime reads Cloudflare's RFC 3339 timestamps; an absent or unreadable
// one is zero, which reads as "none set".
func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return at.UTC()
}

// PermissionGroup is one permission an account token can be granted.
type PermissionGroup struct {
	ID         string
	Name       string
	Scopes     []string
	Selectable bool
}

// PermissionGroups reads every permission group an account token can name.
func (c *Client) PermissionGroups(ctx context.Context) ([]PermissionGroup, error) {
	seen := map[string]bool{}
	var out []PermissionGroup
	err := c.list(ctx, c.accountPath("/tokens/permission_groups"), nil, func(raw json.RawMessage) (int, error) {
		var rows []struct {
			ID         string   `json:"id"`
			Name       string   `json:"name"`
			Scopes     []string `json:"scopes"`
			Selectable *bool    `json:"is_selectable"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return 0, err
		}
		for _, row := range rows {
			if row.ID == "" || seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			out = append(out, PermissionGroup{
				ID: row.ID, Name: row.Name, Scopes: row.Scopes,
				// ABSENT IS SELECTABLE: only an explicit false marks a
				// group a token cannot be granted.
				Selectable: row.Selectable == nil || *row.Selectable,
			})
		}
		return len(rows), nil
	})
	return out, err
}

// Zone is one zone in the account.
type Zone struct {
	ID     string
	Name   string
	Status string
}

// Zones reads every zone in the account the token can see.
func (c *Client) Zones(ctx context.Context) ([]Zone, error) {
	seen := map[string]bool{}
	var out []Zone
	query := url.Values{"account.id": {c.account}}
	err := c.list(ctx, "/zones", query, func(raw json.RawMessage) (int, error) {
		var rows []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return 0, err
		}
		for _, row := range rows {
			if row.ID == "" || seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			out = append(out, Zone(row))
		}
		return len(rows), nil
	})
	return out, err
}

// Policy is one statement of a token's policy. Resources is Cloudflare's own
// shape: resource keys mapped to "*" or to a nested map of the same.
type Policy struct {
	Effect           string         `json:"effect"`
	PermissionGroups []GroupRef     `json:"permission_groups"`
	Resources        map[string]any `json:"resources"`
}

// GroupRef names a permission group in a policy.
type GroupRef struct {
	ID string `json:"id"`
}

// Token is an account API token, without its value.
type Token struct {
	ID        string
	Name      string
	Status    string
	ExpiresOn time.Time
	Policies  []Policy
}

// TokenSpec is what a token is written with.
type TokenSpec struct {
	Name      string
	Policies  []Policy
	ExpiresOn time.Time
	// Status is "active" or "disabled"; empty leaves it to Cloudflare on a
	// create.
	Status string
}

// tokenBody is a spec as Cloudflare's create and update take it. Never an IP
// condition: Cloudflare's MCP server refuses a token that carries one.
func (s TokenSpec) body() map[string]any {
	out := map[string]any{"name": s.Name, "policies": s.Policies}
	if !s.ExpiresOn.IsZero() {
		out["expires_on"] = s.ExpiresOn.UTC().Format(time.RFC3339)
	}
	if s.Status != "" {
		out["status"] = s.Status
	}
	return out
}

// tokenRow is a token as Cloudflare serves it.
type tokenRow struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	ExpiresOn string   `json:"expires_on"`
	Policies  []Policy `json:"policies"`
	Value     string   `json:"value"`
}

func (r tokenRow) token() Token {
	return Token{
		ID: r.ID, Name: r.Name, Status: r.Status,
		ExpiresOn: parseTime(r.ExpiresOn), Policies: r.Policies,
	}
}

// Token reads one account token by ID, reporting whether it exists. A read
// that failed is an error, never an absent token.
func (c *Client) Token(ctx context.Context, id string) (Token, bool, error) {
	if strings.TrimSpace(id) == "" {
		return Token{}, false, errors.New("cloudflare: a token read needs an ID")
	}
	var row tokenRow
	_, err := c.do(ctx, http.MethodGet, c.accountPath("/tokens/"+url.PathEscape(id)), nil, nil, &row)
	switch {
	case Status(err) == http.StatusNotFound:
		return Token{}, false, nil
	case err != nil:
		return Token{}, false, err
	}
	return row.token(), true, nil
}

// Tokens reads every account token, without values.
func (c *Client) Tokens(ctx context.Context) ([]Token, error) {
	seen := map[string]bool{}
	var out []Token
	err := c.list(ctx, c.accountPath("/tokens"), nil, func(raw json.RawMessage) (int, error) {
		var rows []tokenRow
		if err := json.Unmarshal(raw, &rows); err != nil {
			return 0, err
		}
		for _, row := range rows {
			if row.ID == "" || seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			out = append(out, row.token())
		}
		return len(rows), nil
	})
	return out, err
}

// CreateToken mints an account token and returns it with its value, which
// Cloudflare shows exactly once.
func (c *Client) CreateToken(ctx context.Context, spec TokenSpec) (Token, string, error) {
	var row tokenRow
	if _, err := c.do(ctx, http.MethodPost, c.accountPath("/tokens"), nil, spec.body(), &row); err != nil {
		return Token{}, "", err
	}
	if row.ID == "" {
		return Token{}, "", errors.New("cloudflare: a token was created and Cloudflare returned no ID for it")
	}
	return row.token(), row.Value, nil
}

// UpdateToken rewrites a token's name, policies, expiry and status. The whole
// spec is sent: Cloudflare's update replaces rather than merges.
func (c *Client) UpdateToken(ctx context.Context, id string, spec TokenSpec) (Token, error) {
	var row tokenRow
	if _, err := c.do(ctx, http.MethodPut, c.accountPath("/tokens/"+url.PathEscape(id)), nil, spec.body(), &row); err != nil {
		return Token{}, err
	}
	return row.token(), nil
}

// RollToken replaces a token's value, which stops the old one working, and
// returns the new value.
func (c *Client) RollToken(ctx context.Context, id string) (string, error) {
	var value string
	_, err := c.do(ctx, http.MethodPut, c.accountPath("/tokens/"+url.PathEscape(id)+"/value"), nil,
		map[string]any{}, &value)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", errors.New("cloudflare: a token was rolled and Cloudflare returned no value for it")
	}
	return value, nil
}

// DeleteToken deletes a token. One already gone is success, so a retried
// teardown does not fail on what an earlier attempt did.
func (c *Client) DeleteToken(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, c.accountPath("/tokens/"+url.PathEscape(id)), nil, nil, nil)
	if Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}
