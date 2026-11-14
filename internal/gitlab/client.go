package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// The REST client.
//
// ONE credential, the engine's. Unlike the tracker there is no per-seat read
// path here: the only thing the engine reads is a thread's participant list,
// and who participates in a merge request is not a per-viewer fact — a seat's
// own token would return the same list and cost a credential per seat to
// find out.

// APIPath is the REST prefix.
const APIPath = "/api/v4"

// ClientTimeout bounds one request.
//
// SHORTER than the tracker's, and the difference is where the call sits: a
// participants lookup runs INSIDE the inbound consumer, before a delivery is
// acked, so a slow instance stalls the fleet's whole notification path
// rather than one turn. Ten seconds is generous for a single-page read from
// a code host and short enough that a hung instance costs one round of
// deliveries rather than a redelivery storm.
const ClientTimeout = 10 * time.Second

// Client is one authenticated GitLab session.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// ClientOptions configure a [Client].
type ClientOptions struct {
	// URL is the instance base, with or without the API path.
	URL string

	// Token is a personal or group access token with `api` scope.
	Token string

	HTTP *http.Client
}

// NewClient builds a client.
func NewClient(opts ClientOptions) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(opts.URL), "/")
	if base == "" {
		return nil, fmt.Errorf("gitlab: no instance url")
	}
	// An operator writes the instance url; a provisioner writes the api
	// base. Accepting both and normalising here is the difference between
	// a working config and a 404 on every call with no clue why.
	base = strings.TrimSuffix(base, APIPath)
	if strings.TrimSpace(opts.Token) == "" {
		return nil, fmt.Errorf("gitlab: no access token")
	}
	client := opts.HTTP
	if client == nil {
		client = httpx.Client(ClientTimeout)
	}
	return &Client{base: base, token: strings.TrimSpace(opts.Token), http: client}, nil
}

// URL is the instance base, without the API path.
func (c *Client) URL() string { return c.base }

// open sends one GET and answers its response, open, for a status below 400.
// The caller closes the body.
func (c *Client) open(ctx context.Context, path string, params url.Values, accept string) (*http.Response, error) {
	target := c.base + APIPath + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("gitlab: %s: %w", path, err)
	}
	// PRIVATE-TOKEN rather than a bearer, which GitLab accepts for both a
	// personal access token and a group one. Bearer is for OAuth tokens
	// only and 401s on a PAT — the same credential, rejected purely on
	// which header carried it.
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", accept)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab: %s: %w", path, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		// TYPED, like the write half's. A caller deciding what a refusal
		// means — 404 is "not there yet", 403 is "this credential
		// cannot" — would otherwise substring-match a message whose
		// wording differs by GitLab version and by locale.
		return nil, &APIError{
			Method: http.MethodGet, Path: path, Status: resp.StatusCode,
			Detail: httpx.ReadRefusal(resp),
		}
	}
	return resp, nil
}

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	_, err := c.getPage(ctx, path, params, out)
	return err
}

// getPage is get for one page of a listing GitLab pages by keyset: it also
// answers what to add to the query for the page after it, nil at the last.
func (c *Client) getPage(ctx context.Context, path string, params url.Values, out any) (url.Values, error) {
	resp, err := c.open(ctx, path, params, "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, httpx.MaxResponseBody)).Decode(out); err != nil {
		return nil, fmt.Errorf("gitlab: decode %s: %w", path, err)
	}
	return nextPage(resp.Header), nil
}

// nextPage reads where a listing goes on from its response's headers: the
// page token of the `next` link a keyset listing sends, or the page number an
// instance that pages by offset sends instead. Nil is the last page.
func nextPage(h http.Header) url.Values {
	for _, link := range strings.Split(h.Get("Link"), ",") {
		target, rel, _ := strings.Cut(link, ";")
		if !strings.Contains(rel, `rel="next"`) {
			continue
		}
		u, err := url.Parse(strings.Trim(strings.TrimSpace(target), "<>"))
		if err != nil {
			continue
		}
		for _, key := range []string{"page_token", "page"} {
			if v := u.Query().Get(key); v != "" {
				return url.Values{key: {v}}
			}
		}
	}
	if n := strings.TrimSpace(h.Get("X-Next-Page")); n != "" {
		return url.Values{"page": {n}}
	}
	return nil
}

// getRaw reads a response's body as it is, at most limit bytes of it, and
// reports a body past limit, of which it answers nothing.
func (c *Client) getRaw(ctx context.Context, path string, limit int64) (raw []byte, oversize bool, err error) {
	resp, err := c.open(ctx, path, nil, "application/octet-stream")
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, false, fmt.Errorf("gitlab: read %s: %w", path, err)
	}
	if int64(len(raw)) > limit {
		return nil, true, nil
	}
	return raw, false, nil
}

// Me is the account this credential authenticates as.
//
// The boot identity check: a token that resolves proves the credential works
// AND names the account, which is what stops the engine registering itself
// under a username somebody assumed.
func (c *Client) Me(ctx context.Context) (string, error) {
	var out struct {
		Username string `json:"username"`
	}
	if err := c.get(ctx, "/user", nil, &out); err != nil {
		return "", err
	}
	return strings.ToLower(out.Username), nil
}

// ParticipantsOf implements [Participants].
//
// Everyone taking part: author, assignees, reviewers, commenters, and anyone
// previously mentioned — GitLab's own definition, computed server-side,
// which is precisely why this is a read rather than something derived from
// the payload.
//
// ONE PAGE, never a cursor walk. A thread with more than a hundred
// participants is a thread where notifying all of them is the wrong
// behaviour anyway, and draining every page would turn one bounded call on
// the inbound consumer's hot path into an unbounded crawl.
func (c *Client) ParticipantsOf(ctx context.Context, projectID int, kind string, iid int) ([]string, error) {
	collection := "issues"
	if kind == "merge_request" {
		collection = "merge_requests"
	}
	path := "/projects/" + strconv.Itoa(projectID) + "/" + collection +
		"/" + strconv.Itoa(iid) + "/participants"

	var rows []struct {
		Username string `json:"username"`
	}
	params := url.Values{"per_page": {"100"}}
	if err := c.get(ctx, path, params, &rows); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		// LOWERCASED at the boundary, like every other username in the
		// integration: GitLab preserves the case an account was created
		// with, and a participant compared raw against a lowercased
		// mention is one person the engine sees as two.
		if name := strings.ToLower(strings.TrimSpace(row.Username)); name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// Lookup adapts a client to the parser's [Participants] seam.
//
// A nil Client REPORTS an error rather than dereferencing: a company with no
// engine token still routes from what its payloads name, and the parser
// already treats a failed lookup as "fall back to the assignees". A panic
// here would turn that documented degradation into a dead inbound consumer.
type Lookup struct{ Client *Client }

// Of implements [Participants].
func (l Lookup) Of(ctx context.Context, projectID int, kind string, iid int) ([]string, error) {
	if l.Client == nil {
		return nil, fmt.Errorf("gitlab: no client")
	}
	return l.Client.ParticipantsOf(ctx, projectID, kind, iid)
}
