package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The REST client.
//
// # One credential per question, and they are different questions
//
// The ENGINE's credential answers "who is taking part in this thread" — a
// fact about the thread rather than about the viewer, so a seat's own token
// would return the same list and cost a credential per seat to find out.
//
// A SEAT's own credential answers "which account is this seat" and nothing
// else. It is used once per distinct credential, at boot, and the answer is
// cached against the credential rather than the seat.
//
// # Why the API version is pinned
//
// GitHub versions its REST API by date and serves the newest to a client
// that names none. That makes an unpinned client's behaviour change under a
// deployment that did not — the failure mode a versioned API exists to
// prevent — so the header is sent on every request and the version is a
// constant this build states.

// APIVersion is the REST API version every request names.
//
// Bumping it is a deliberate change with its own commit: GitHub's breaking
// changes land between versions, so the value is the one this build's field
// expectations were written against.
const APIVersion = "2022-11-28"

// Accept is the media type that selects the JSON representation.
const Accept = "application/vnd.github+json"

// ClientTimeout bounds one request.
//
// SHORT, and the reason is where the call sits: a participants lookup runs
// INSIDE the inbound consumer, before a delivery is acked, so a slow API
// stalls the fleet's whole notification path rather than one turn. Ten
// seconds is generous for a single-page read and short enough that a
// degraded API costs one round of deliveries rather than a redelivery storm.
const ClientTimeout = 10 * time.Second

// PageSize is the per-page count on every list this client reads.
//
// GitHub's own maximum. ONE PAGE, never a cursor walk: a thread with more
// than a hundred commenters is one where notifying all of them is the wrong
// behaviour anyway, and draining every page would turn a bounded call on the
// inbound hot path into an unbounded crawl.
const PageSize = 100

// Client is one authenticated GitHub session.
type Client struct {
	base  string
	web   string
	token string
	http  *http.Client
}

// ClientOptions configure a [Client].
type ClientOptions struct {
	// APIBase is the REST base — https://api.github.com, or an Enterprise
	// Server's /api/v3. Empty means github.com.
	APIBase string

	// WebBase is the base a shareable link is built on. Empty means
	// github.com.
	WebBase string

	// Token is a personal access token, a fine-grained token, or an app
	// installation token. All three authenticate as a bearer.
	Token string

	HTTP *http.Client
}

// The github.com addresses, which are two different hosts rather than one
// host with two paths.
const (
	defaultAPIBase = "https://api.github.com"
	defaultWebBase = "https://github.com"
)

// NewClient builds a client.
func NewClient(opts ClientOptions) (*Client, error) {
	if strings.TrimSpace(opts.Token) == "" {
		return nil, fmt.Errorf("github: no access token")
	}
	base := strings.TrimRight(strings.TrimSpace(opts.APIBase), "/")
	if base == "" {
		base = defaultAPIBase
	}
	web := strings.TrimRight(strings.TrimSpace(opts.WebBase), "/")
	if web == "" {
		web = defaultWebBase
	}
	client := opts.HTTP
	if client == nil {
		client = &http.Client{Timeout: ClientTimeout}
	}
	return &Client{
		base: base, web: web,
		token: strings.TrimSpace(opts.Token), http: client,
	}, nil
}

// APIBase is the REST base this client calls.
func (c *Client) APIBase() string { return c.base }

// WebBase is the base a shareable link is built on.
func (c *Client) WebBase() string { return c.web }

// APIError is a refusal from the API.
//
// TYPED, so a caller deciding what a refusal MEANS — 404 is "not there or
// not visible to this credential", 403 is "this credential may not", 401 is
// "this credential is dead" — does not substring-match a message whose
// wording GitHub changes freely.
type APIError struct {
	Method string
	Path   string
	Status int
	Detail string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("github: %s %s: %d", e.Method, e.Path, e.Status)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// NotFound reports a 404, which on GitHub means "absent OR invisible to this
// credential" and never distinguishes the two.
//
// That conflation is GitHub's own and it is deliberate on their side: a
// private repository answers 404 rather than 403 so that a probe cannot
// enumerate what exists. A caller acting on it has to say both.
func (e *APIError) NotFound() bool { return e.Status == http.StatusNotFound }

// Forbidden reports a credential that authenticated and may not do this.
func (e *APIError) Forbidden() bool { return e.Status == http.StatusForbidden }

func (c *Client) do(ctx context.Context, method, path string, params url.Values, in, out any) error {
	target := c.base + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("github: encode %s: %w", path, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return fmt.Errorf("github: %s: %w", path, err)
	}
	// A BEARER, which covers all three token kinds. The older `token`
	// scheme works for a classic PAT and not for an installation token —
	// the same credential accepted or refused purely on which word carried
	// it, which is a failure with no message that names the cause.
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", Accept)
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &APIError{
			Method: method, Path: path, Status: resp.StatusCode,
			Detail: strings.TrimSpace(string(detail)),
		}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("github: decode %s: %w", path, err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, params, nil, out)
}

// Me is the account this credential authenticates as, lowercased.
//
// The boot identity check: a token that resolves proves the credential works
// AND names the account, which is what stops the engine registering a seat
// under a login somebody assumed.
func (c *Client) Me(ctx context.Context) (string, error) {
	var out struct {
		Login string `json:"login"`
	}
	if err := c.get(ctx, "/user", nil, &out); err != nil {
		return "", err
	}
	if out.Login == "" {
		// An INSTALLATION token authenticates as an app rather than a
		// user and /user answers 403 for it — but a token that answered
		// 200 with no login is a shape nothing documents, and
		// registering a seat under "" would make it the routing target
		// for every event whose login field was missing.
		return "", fmt.Errorf(
			"github: /user answered with no login, so there is no account to " +
				"register this seat under — an installation token authenticates " +
				"as an app rather than as a person and cannot hold a seat's " +
				"identity")
	}
	return strings.ToLower(out.Login), nil
}

// ParticipantsOf is everyone who has COMMENTED on an issue or pull request,
// plus everyone who has REVIEWED a pull request.
//
// # Why this is computed rather than read
//
// GitHub has no participants endpoint. Its own notification rule is that you
// are subscribed once you author, are assigned, are mentioned, comment or
// review — and of those five, three are already in the webhook payload and
// one is in the text. What is left, and what this reads, is the two that are
// only in the API.
//
// TWO CALLS FOR A PULL REQUEST rather than one, because GitHub keeps a pull
// request's REVIEWS in a different collection from its comments and a review
// with no comment appears in neither the other. A reviewer who approved
// without writing anything is exactly the person who should hear that the
// author pushed again.
//
// The two are read CONCURRENTLY: sequentially they are two round trips on
// the inbound consumer's hot path, and the second does not depend on the
// first.
func (c *Client) ParticipantsOf(ctx context.Context, owner, repo, kind string, number int) ([]string, error) {
	base := "/repos/" + owner + "/" + repo
	item := base + "/issues/" + strconv.Itoa(number)

	type result struct {
		people []string
		err    error
	}
	comments := make(chan result, 1)
	go func() {
		people, err := c.authorsOf(ctx, item+"/comments")
		comments <- result{people, err}
	}()

	var reviewers []string
	var reviewErr error
	if kind == "pull_request" {
		reviewers, reviewErr = c.authorsOf(ctx,
			base+"/pulls/"+strconv.Itoa(number)+"/reviews")
	}
	commented := <-comments

	if commented.err != nil {
		return nil, commented.err
	}
	if reviewErr != nil {
		return nil, reviewErr
	}
	return append(commented.people, reviewers...), nil
}

// authorsOf reads one page of a comment or review collection and returns its
// authors, lowercased.
func (c *Client) authorsOf(ctx context.Context, path string) ([]string, error) {
	var rows []struct {
		User user `json:"user"`
	}
	params := url.Values{"per_page": {strconv.Itoa(PageSize)}}
	if err := c.get(ctx, path, params, &rows); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		// LOWERCASED at the boundary, like every other login in the
		// integration: GitHub preserves the case an account was created
		// with, and an author compared raw against a lowercased mention
		// is one person the engine sees as two.
		if name := strings.ToLower(strings.TrimSpace(row.User.Login)); name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// Lookup adapts a client to the parser's [Participants] seam.
//
// A nil Client REPORTS an error rather than dereferencing: a company with no
// engine token still routes from what its payloads name, and the parser
// already treats a failed lookup as "fall back to the author and assignees".
// A panic here would turn that documented degradation into a dead inbound
// consumer.
type Lookup struct{ Client *Client }

// Of implements [Participants].
func (l Lookup) Of(ctx context.Context, owner, repo, kind string, number int) ([]string, error) {
	if l.Client == nil {
		return nil, fmt.Errorf("github: no client")
	}
	return l.Client.ParticipantsOf(ctx, owner, repo, kind, number)
}
