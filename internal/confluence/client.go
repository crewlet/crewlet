// Package confluence is the Confluence integration: the company's knowledge
// base, and the inbound edge that turns a page change into a woken seat.
//
// # It is the KNOWLEDGE backend, and that is what makes it different
//
// The other vendors route events. This one also answers the Plan phase's
// "what do we already know about this" — a live CQL search at retrieval
// time, run as the ASKING SEAT wherever that seat has its own Atlassian
// credential, so Confluence enforces its own page permissions and the engine
// keeps no restricted-page bookkeeping. See [Searcher].
package confluence

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("confluence")

// Backend is the transport name, and the party registry's namespace for a
// Confluence account id.
//
// The SAME account id Jira uses — one Atlassian identity covers both
// products — but a separate namespace, because a company can serve one and
// refuse the other and a shared namespace would make that impossible to
// reason about.
const Backend = "confluence"

// APIPath is the REST prefix under a base address.
const APIPath = "/rest/api"

// wikiPrefix is what a Cloud SITE address needs and a gateway address does
// not.
//
// A Cloud site serves Confluence under /wiki (https://acme.atlassian.net/wiki
// /rest/api); the api.atlassian.com gateway already addresses the product, so
// adding it there produces a 404 on every call. Data Center serves it at the
// root. Getting this wrong is the single most likely way to have a correct
// credential fail everything.
const wikiPrefix = "/wiki"

// ClientTimeout bounds one request.
//
// The search runs INSIDE the Plan phase, before a model call the turn is
// waiting on, so a slow wiki costs the turn directly. Fifteen seconds is
// generous for one CQL page and short enough that a hung instance degrades
// to an empty knowledge block rather than stalling a seat.
const ClientTimeout = 15 * time.Second

// Client is one authenticated Confluence session.
type Client struct {
	base string
	auth string
	http *http.Client
}

// ClientOptions configure a [Client].
type ClientOptions struct {
	// URL is a direct instance address — a Data Center base, or a Cloud
	// site — or an api.atlassian.com gateway built from a cloud id.
	URL string

	// Email switches authentication to Basic base64(email:token), which
	// Cloud requires. Empty sends a bearer token, which is what a Data
	// Center personal access token wants.
	Email string
	Token string

	HTTP *http.Client
}

// NewClient builds a client.
func NewClient(opts ClientOptions) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(opts.URL), "/")
	if base == "" {
		return nil, fmt.Errorf("confluence: no instance url")
	}
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		return nil, fmt.Errorf("confluence: no api token")
	}
	client := opts.HTTP
	if client == nil {
		client = &http.Client{Timeout: ClientTimeout}
	}
	return &Client{
		base: RESTBase(base),
		auth: authHeader(strings.TrimSpace(opts.Email), token),
		http: client,
	}, nil
}

// RESTBase is the address every call is made against, /wiki included where
// the deployment needs it.
//
// Three shapes, and the difference is not cosmetic:
//
//   - an api.atlassian.com gateway already names the product, so /wiki
//     there is a 404 on every call;
//   - a Cloud SITE serves Confluence under /wiki, and without it every call
//     lands on the Jira side of the same host;
//   - Data Center serves it at the root.
//
// A base that already ends in /wiki is left alone, because an operator who
// wrote the full address is not wrong.
func RESTBase(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	switch {
	case base == "":
		return ""
	case strings.HasSuffix(base, wikiPrefix):
		return base
	case strings.Contains(base, "api.atlassian.com/ex/confluence"):
		return base
	case isCloudSite(base):
		return base + wikiPrefix
	default:
		return base
	}
}

// isCloudSite reports an Atlassian-hosted site address.
func isCloudSite(base string) bool {
	parsed, err := url.Parse(base)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, suffix := range []string{".atlassian.net", ".jira.com"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// authHeader chooses between Cloud's Basic scheme and a bearer token.
//
// The presence of an EMAIL is the whole discriminator, and it is the field
// an operator already has to set correctly for their deployment: Cloud
// rejects a bare bearer API token and Data Center rejects Basic with an
// empty user — the same credential, refused purely on which header carried
// it.
func authHeader(email, token string) string {
	if email == "" {
		return "Bearer " + token
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token))
}

// URL is the REST base this client reads.
func (c *Client) URL() string { return c.base }

// APIError is a refusal from the instance, typed.
type APIError struct {
	Method string
	Path   string
	Status int
	Detail string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("confluence: %s %s: %d", e.Method, e.Path, e.Status)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

func (c *Client) do(ctx context.Context, method, path string, params url.Values, in, out any) error {
	target := c.base + APIPath + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	var payload io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("confluence: encode %s: %w", path, err)
		}
		payload = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return fmt.Errorf("confluence: %s: %w", path, err)
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("confluence: %s: %w", path, err)
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
		return fmt.Errorf("confluence: decode %s: %w", path, err)
	}
	return nil
}

// Me is the account this credential authenticates as.
func (c *Client) Me(ctx context.Context) (string, error) {
	var out struct {
		AccountID string `json:"accountId"`
		Username  string `json:"username"`
		UserKey   string `json:"userKey"`
	}
	if err := c.do(ctx, http.MethodGet, "/user/current", nil, nil, &out); err != nil {
		return "", err
	}
	return firstOf(out.AccountID, out.Username, out.UserKey), nil
}

// Page is one page as this integration reads it.
type Page struct {
	ID    string
	Title string
	Space string
	// Body is the STORAGE FORMAT, which is XHTML rather than the rendered
	// view. Everything downstream — the snippet, the skill decoder —
	// flattens it; nothing renders it.
	Body string
	// Ancestors are the parent chain, outermost first. It is what the
	// auto-draft exclusion reads.
	Ancestors []string
	Version   int
}

// pageWire is the shape a page arrives in.
type pageWire struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type"`
	Space struct {
		Key string `json:"key"`
	} `json:"space"`
	Body struct {
		Storage struct {
			Value string `json:"value"`
		} `json:"storage"`
	} `json:"body"`
	Ancestors []struct {
		Title string `json:"title"`
	} `json:"ancestors"`
	Version struct {
		Number int `json:"number"`
	} `json:"version"`
	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
}

func (w pageWire) page() Page {
	page := Page{
		ID: w.ID, Title: w.Title, Space: w.Space.Key,
		Body: w.Body.Storage.Value, Version: w.Version.Number,
	}
	for _, ancestor := range w.Ancestors {
		page.Ancestors = append(page.Ancestors, ancestor.Title)
	}
	return page
}

// expandFields is what every read asks the server to inline.
//
// ONE EXPAND LIST, because the three readers below need the same three
// things and a search that forgot `ancestors` would silently stop excluding
// auto-drafts — a filter that quietly matches nothing looks exactly like a
// knowledge base with no drafts in it.
const expandFields = "body.storage,space,ancestors,version"

// Search runs one CQL query.
//
// The CQL is built by [BuildCQL] rather than here, so the one place that
// decides what a seat may read is the searcher rather than the transport.
func (c *Client) Search(ctx context.Context, cql string, limit int) ([]Page, error) {
	if strings.TrimSpace(cql) == "" {
		return nil, fmt.Errorf("confluence: empty cql")
	}
	params := url.Values{
		"cql":    {cql},
		"limit":  {strconv.Itoa(limit)},
		"expand": {expandFields},
	}
	var out struct {
		Results []pageWire `json:"results"`
	}
	if err := c.do(ctx, http.MethodGet, "/content/search", params, nil, &out); err != nil {
		return nil, err
	}
	pages := make([]Page, 0, len(out.Results))
	for _, row := range out.Results {
		pages = append(pages, row.page())
	}
	return pages, nil
}

// PageByID reads one page whole.
func (c *Client) PageByID(ctx context.Context, id string) (Page, error) {
	var out pageWire
	params := url.Values{"expand": {expandFields}}
	if err := c.do(ctx, http.MethodGet, "/content/"+url.PathEscape(id), params, nil, &out); err != nil {
		return Page{}, err
	}
	return out.page(), nil
}

// PagesIn lists every page in a space.
//
// PAGED TO EXHAUSTION, unlike the search: this walks the tool-skills
// container, and a partial walk silently DELETES every skill it did not
// reach — the registry replace is wholesale. A truncated read has to be an
// error, and the only way to know it was truncated is to keep asking.
func (c *Client) PagesIn(ctx context.Context, space string) ([]Page, error) {
	space = strings.ToUpper(strings.TrimSpace(space))
	if space == "" {
		return nil, fmt.Errorf("confluence: no space key")
	}
	var out []Page
	for start := 0; ; start += pageWalkSize {
		params := url.Values{
			"spaceKey": {space},
			"type":     {"page"},
			"limit":    {strconv.Itoa(pageWalkSize)},
			"start":    {strconv.Itoa(start)},
			"expand":   {expandFields},
		}
		var batch struct {
			Results []pageWire `json:"results"`
			Size    int        `json:"size"`
		}
		if err := c.do(ctx, http.MethodGet, "/content", params, nil, &batch); err != nil {
			return nil, err
		}
		for _, row := range batch.Results {
			out = append(out, row.page())
		}
		if len(batch.Results) < pageWalkSize {
			return out, nil
		}
		if len(out) >= pageWalkCeiling {
			// A CONTAINER THIS SIZE IS NOT A SKILLS CONTAINER. Walking
			// on would spend minutes and produce a catalogue no prompt
			// could carry, so the honest answer is to refuse and say
			// which space to split.
			return nil, fmt.Errorf(
				"confluence: space %s holds more than %d pages, which is not a "+
					"tool-skills container — point integrations.confluence."+
					"skills_space at a space that holds only skills", space, pageWalkCeiling)
		}
	}
}

const (
	// pageWalkSize is one page of the content listing. Confluence's own
	// ceiling for this endpoint is 100 and asking for more is silently
	// truncated, which would make a partial walk look complete.
	pageWalkSize = 100

	// pageWalkCeiling bounds the skills walk. Every admitted skill is
	// prompt text a seat carries, so a container of thousands is a
	// misconfiguration rather than a large company.
	pageWalkCeiling = 1000
)

// CreatePage posts a new page into a space, optionally under a parent.
func (c *Client) CreatePage(ctx context.Context, space, title, storage, parentID string) (Page, error) {
	body := map[string]any{
		"type":  "page",
		"title": title,
		"space": map[string]any{"key": strings.ToUpper(strings.TrimSpace(space))},
		"body": map[string]any{
			"storage": map[string]any{"value": storage, "representation": "storage"},
		},
	}
	if parentID != "" {
		body["ancestors"] = []any{map[string]any{"id": parentID}}
	}
	var out pageWire
	if err := c.do(ctx, http.MethodPost, "/content", nil, body, &out); err != nil {
		return Page{}, err
	}
	return out.page(), nil
}

// UpdatePage replaces a page's body, bumping its version.
//
// CONFLUENCE REQUIRES THE NEXT VERSION NUMBER and refuses anything else,
// which is optimistic concurrency rather than bookkeeping: two writers who
// both read version 4 cannot both write version 5, so the second is refused
// rather than silently overwriting the first.
func (c *Client) UpdatePage(ctx context.Context, id, title, storage string, version int) (Page, error) {
	body := map[string]any{
		"id":      id,
		"type":    "page",
		"title":   title,
		"version": map[string]any{"number": version + 1},
		"body": map[string]any{
			"storage": map[string]any{"value": storage, "representation": "storage"},
		},
	}
	var out pageWire
	if err := c.do(ctx, http.MethodPut, "/content/"+url.PathEscape(id), nil, body, &out); err != nil {
		return Page{}, err
	}
	return out.page(), nil
}

// MovePage re-parents a page, which is how a lead publishes an auto-drafted
// skill: moving it out of the drafts parent IS the review.
func (c *Client) MovePage(ctx context.Context, id, title string, version int, parentID string) error {
	body := map[string]any{
		"id":      id,
		"type":    "page",
		"title":   title,
		"version": map[string]any{"number": version + 1},
	}
	if parentID != "" {
		body["ancestors"] = []any{map[string]any{"id": parentID}}
	} else {
		// An EMPTY list moves the page to the space root. Omitting the
		// key leaves the parent alone, which would make "publish" a
		// no-op — the failure mode this method exists to avoid.
		body["ancestors"] = []any{}
	}
	return c.do(ctx, http.MethodPut, "/content/"+url.PathEscape(id), nil, body, nil)
}

// SpaceExists reports whether the instance has a space, and whether this
// credential can read it.
//
// TWO QUESTIONS, ONE ANSWER, deliberately: from the importer's point of view
// a space it cannot read and a space that does not exist have the same
// consequence — nothing can be published there — and the error carries which
// it was.
func (c *Client) SpaceExists(ctx context.Context, key string) (bool, error) {
	key = strings.ToUpper(strings.TrimSpace(key))
	if key == "" {
		return false, fmt.Errorf("confluence: no space key")
	}
	err := c.do(ctx, http.MethodGet, "/space/"+url.PathEscape(key), nil, nil, nil)
	if err == nil {
		return true, nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

// PageByTitle finds one page in a space by exact title, or reports absent.
func (c *Client) PageByTitle(ctx context.Context, space, title string) (Page, bool, error) {
	params := url.Values{
		"spaceKey": {strings.ToUpper(strings.TrimSpace(space))},
		"title":    {title},
		"type":     {"page"},
		"expand":   {expandFields},
	}
	var out struct {
		Results []pageWire `json:"results"`
	}
	if err := c.do(ctx, http.MethodGet, "/content", params, nil, &out); err != nil {
		return Page{}, false, err
	}
	if len(out.Results) == 0 {
		return Page{}, false, nil
	}
	return out.Results[0].page(), true, nil
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
