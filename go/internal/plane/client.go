package plane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The REST client.
//
// One per SEAT where a seat has its own API key, and one for the engine
// besides. The difference is not incidental: a search run under a seat's own
// key is bounded by what that seat's account can read, which is the whole
// basis of the unscoped-search rule — and a shared engine key run on a
// seat's behalf would show it pages its own account never could.

// APIPath is the REST prefix.
const APIPath = "/api/v1"

// SearchPageSize is the endpoint's accepted ceiling for one search.
//
// Clamped rather than passed through, because asking for more is a 400 —
// which turns a caller's over-eager limit into no knowledge at all rather
// than into slightly less of it.
const SearchPageSize = 100

// ClientTimeout bounds one request.
const ClientTimeout = 30 * time.Second

// Client is one authenticated Plane session.
type Client struct {
	base      string
	workspace string
	key       string
	http      *http.Client
}

// ClientOptions configure a [Client].
type ClientOptions struct {
	URL       string
	Workspace string
	// APIKey authenticates. A seat's own where it has one; the engine's
	// otherwise — and which it is decides what a search may see.
	APIKey string
	HTTP   *http.Client
}

// NewClient builds a client.
func NewClient(opts ClientOptions) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(opts.URL), "/")
	if base == "" {
		return nil, fmt.Errorf("plane: no instance url")
	}
	if opts.Workspace == "" {
		return nil, fmt.Errorf("plane: no workspace")
	}
	if opts.APIKey == "" {
		return nil, fmt.Errorf("plane: no api key")
	}
	c := &Client{base: base, workspace: opts.Workspace, key: opts.APIKey, http: opts.HTTP}
	if c.http == nil {
		c.http = &http.Client{Timeout: ClientTimeout}
	}
	return c, nil
}

// Workspace is the slug this client reads.
func (c *Client) Workspace() string { return c.workspace }

// URL is the instance address.
func (c *Client) URL() string { return c.base }

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	target := c.base + APIPath + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("plane: %s: %w", path, err)
	}
	req.Header.Set("X-API-Key", c.key)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("plane: %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("plane: %s: %d: %s", path, resp.StatusCode,
			strings.TrimSpace(string(detail)))
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("plane: decode %s: %w", path, err)
	}
	return nil
}

// rows unwraps the cursor envelope, tolerating a bare list.
//
// The endpoint returns {results, next_cursor} on some builds and a plain
// array on others, and a reader that knew only one shape would come back
// empty on the other — silently, because "no results" is a legitimate answer.
func rows(payload json.RawMessage) []map[string]any {
	var list []map[string]any
	if err := json.Unmarshal(payload, &list); err == nil {
		return list
	}
	var envelope struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil {
		return envelope.Results
	}
	return nil
}

// Project is one project in the workspace.
type Project struct {
	ID string `json:"id"`
	// Identifier is the short key people use — "ENG".
	Identifier string `json:"identifier"`
	Name       string `json:"name"`
}

// Projects lists the workspace's projects.
func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	var payload json.RawMessage
	if err := c.get(ctx, "/workspaces/"+url.PathEscape(c.workspace)+"/projects/", nil, &payload); err != nil {
		return nil, err
	}
	var out []Project
	for _, row := range rows(payload) {
		out = append(out, Project{
			ID: str(row, "id"), Identifier: str(row, "identifier"), Name: str(row, "name"),
		})
	}
	return out, nil
}

// SubscriberIDs lists the users following a work item.
func (c *Client) SubscriberIDs(ctx context.Context, projectID, issueID string) ([]string, error) {
	if projectID == "" || issueID == "" {
		return nil, fmt.Errorf("plane: subscribers need a project and an issue")
	}
	var payload json.RawMessage
	path := "/workspaces/" + url.PathEscape(c.workspace) +
		"/projects/" + url.PathEscape(projectID) +
		"/issues/" + url.PathEscape(issueID) + "/subscribers/"
	if err := c.get(ctx, path, nil, &payload); err != nil {
		return nil, err
	}
	var out []string
	for _, row := range rows(payload) {
		// The row names the subscriber in one of two shapes depending on
		// the build: a `subscriber` foreign key, or an expanded user.
		for _, key := range []string{"subscriber", "subscriber_id", "id"} {
			if id := refID(row[key]); id != "" {
				out = append(out, id)
				break
			}
		}
	}
	return out, nil
}

// Page is one knowledge page.
type Page struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ProjectID   string `json:"project"`
	Description string `json:"description_stripped"`
	UpdatedAt   string `json:"updated_at"`
}

// SearchPages runs the workspace page search.
//
// The query is sent VERBATIM: the server tokenises it and ANDs the tokens,
// each matched case-insensitively against a page's name and its stripped
// body. A client that rewrote it would be guessing at a ranking the server
// already has an opinion about.
//
// ONE REQUEST, never a cursor walk. A search consumer wants the top of the
// window, and draining every result page would turn one bounded call into an
// unbounded crawl on the Plan-phase hot path.
func (c *Client) SearchPages(ctx context.Context, query string, projectIDs []string, limit int) ([]Page, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("per_page", strconv.Itoa(clamp(limit, 1, SearchPageSize)))
	if len(projectIDs) > 0 {
		// Comma-joined UUIDs. Identifiers are a 400 here, which is why
		// the scope is resolved to ids before it ever reaches this call.
		params.Set("projects", strings.Join(projectIDs, ","))
	}
	var payload json.RawMessage
	path := "/workspaces/" + url.PathEscape(c.workspace) + "/pages/search/"
	if err := c.get(ctx, path, params, &payload); err != nil {
		return nil, err
	}
	var out []Page
	for _, row := range rows(payload) {
		out = append(out, Page{
			ID: str(row, "id"), Name: str(row, "name"),
			ProjectID:   pageProjectID(row),
			Description: firstOf(str(row, "description_stripped"), str(row, "description_html")),
			UpdatedAt:   str(row, "updated_at"),
		})
	}
	return out, nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ProjectCache resolves a project UUID to the identifier people use.
//
// A CACHE because the id→identifier mapping is on the routing hot path — a
// busy project produces a webhook a second and each needs the identifier to
// find its lead — while the mapping itself changes about as often as
// somebody creates a project.
//
// A miss REFETCHES, once, behind a floor: an id that is genuinely unknown
// (a project created since the last walk) must resolve, but an id that is
// unknown because it was deleted must not send a request per webhook.
type ProjectCache struct {
	client  *Client
	floor   time.Duration
	now     func() time.Time
	mu      sync.Mutex
	byID    map[string]string
	fetched time.Time
	primed  bool
}

// CacheRefetchFloor is the shortest gap between two full project walks.
//
// A minute, against a mapping that changes when somebody creates a project.
// Short enough that a project made during a demo resolves before anybody
// notices; long enough that a webhook naming a deleted project cannot turn
// into a request per delivery.
const CacheRefetchFloor = time.Minute

// NewProjectCache builds the cache.
func NewProjectCache(c *Client, now func() time.Time) *ProjectCache {
	if now == nil {
		now = time.Now
	}
	return &ProjectCache{client: c, floor: CacheRefetchFloor, now: now, byID: map[string]string{}}
}

// Identifier implements [Projects].
//
// An unresolvable id yields "", which every caller reads as FAIL CLOSED: a
// page whose project is unknown routes to nobody rather than to a guess.
func (p *ProjectCache) Identifier(ctx context.Context, projectID string) string {
	if p == nil || projectID == "" {
		return ""
	}
	projectID = strings.ToLower(projectID)

	p.mu.Lock()
	identifier, known := p.byID[projectID]
	stale := !p.primed || p.now().Sub(p.fetched) >= p.floor
	p.mu.Unlock()
	if known {
		return identifier
	}
	if !stale {
		return ""
	}

	projects, err := p.client.Projects(ctx)
	if err != nil {
		log.Warn("plane_projects_unavailable", "error", err.Error())
		// The fetch is stamped even on failure, so an unreachable
		// instance costs one request per floor rather than one per
		// webhook — which is the difference between a slow recovery
		// and a self-inflicted outage.
		p.mu.Lock()
		p.fetched, p.primed = p.now(), true
		p.mu.Unlock()
		return ""
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, project := range projects {
		if project.ID != "" && project.Identifier != "" {
			p.byID[strings.ToLower(project.ID)] = project.Identifier
		}
	}
	p.fetched, p.primed = p.now(), true
	return p.byID[projectID]
}

// Learn records a mapping seen in a payload, so a webhook that carries both
// halves saves the walk that would otherwise resolve it.
func (p *ProjectCache) Learn(projectID, identifier string) {
	if p == nil || projectID == "" || identifier == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byID[strings.ToLower(projectID)] = identifier
}

// IDsFor resolves identifiers to project UUIDs — the direction a search
// scope needs, since the search endpoint refuses identifiers.
func (p *ProjectCache) IDsFor(ctx context.Context, identifiers []string) []string {
	if p == nil || len(identifiers) == 0 {
		return nil
	}
	// Prime, so an unwarmed cache does not silently resolve nothing and
	// turn a scoped search into an unscoped one.
	p.Identifier(ctx, "prime")

	wanted := make(map[string]bool, len(identifiers))
	for _, id := range identifiers {
		wanted[strings.ToUpper(strings.TrimSpace(id))] = true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for projectID, identifier := range p.byID {
		if wanted[strings.ToUpper(identifier)] {
			out = append(out, projectID)
		}
	}
	return out
}

// SubscriberLookup adapts the client to the parser's [Subscribers] seam.
type SubscriberLookup struct{ Client *Client }

// Of implements [Subscribers].
func (s SubscriberLookup) Of(ctx context.Context, projectID, issueID string) ([]string, error) {
	if s.Client == nil {
		return nil, fmt.Errorf("plane: no client")
	}
	return s.Client.SubscriberIDs(ctx, projectID, issueID)
}
