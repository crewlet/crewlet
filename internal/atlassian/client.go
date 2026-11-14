// Package atlassian provisions one service account per agent in an Atlassian
// organization, and mints the API token that account acts with.
//
// # Why an ORGANIZATION key rather than the site credentials
//
// Everything else this engine does with Atlassian authenticates as a person:
// an email and an API token, against one site, doing what that person may do.
// Creating an identity is not something a person may do, and Atlassian's
// site APIs have no route for it at all — which is why the account each agent
// acts as was, until this package, something an operator made by hand.
//
// The organization API key is a different credential with a different reach.
// It is created in the admin console against the whole organization, it is
// what the console itself calls, and it can create a service account, mint a
// token on one, and delete it again.
//
// # The three services, and why they are not one
//
// Atlassian splits this across three prefixes that answer differently, and
// each of the constants below records what happens when the obvious one is
// used instead. They were established against a live organization; the
// published reference disagrees with two of them.
//
// # The atlas reads the organization's products
//
// The workspace listing that discovers the agents' site is also what the
// atlas reads (ADR-0138, atlas.go): every site as an account, and each
// product as a capability of the account it is on. It is read to its end,
// and nobody a row names is decoded.
package atlassian

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// AdminBaseURL is where every organization-level call goes. It is not the
// company's own site: these APIs are the organization's, and the site is
// discovered through them rather than asked for.
const AdminBaseURL = "https://api.atlassian.com"

// ClientTimeout bounds one call.
const ClientTimeout = 20 * time.Second

// serviceAccountsPath is the collection an agent's identity lives on.
//
// account-management, NOT api-access. Atlassian's published reference
// documents service accounts under api-access; that service serves only
// /count and answers the collection with "Request failed to match any
// route". account-management is what the admin console itself calls.
//
// The two also take different credentials. api-access accepts a scoped key;
// account-management refuses one with 403 whatever scopes it holds, and needs
// an organization API key created WITHOUT scopes. That is why the setup form
// asks for an unscoped key and says so.
const serviceAccountsPath = "/admin/account-management/v1/orgs/%s/service-accounts"

// apiTokensPath mints an API token for one account.
//
// The step Atlassian's own reference calls impossible: its documented token
// endpoints are read and revoke only. The admin console does it through a
// user-manage route, and that route accepts an organization API key, so this
// engine can mint an agent's credential rather than asking a person to create
// one and paste it in.
//
// The expiry field is named `expiry`. Sending `expiresAt`, the name Atlassian
// uses elsewhere, fails with INVALID_EXPIRY and reads as though no expiry had
// been sent at all.
//
//nolint:gosec // a URL template, not a credential
const apiTokensPath = "/users/%s/manage/api-tokens"

// invitePath grants a service account product access.
//
// It is the only route that works, and it is undocumented: the contract comes
// from what the admin console itself calls. Service accounts are not
// directory users, so adding one to a group answers USER_NOT_FOUND, and role
// assignments are read-only.
//
// WITHOUT IT AN ACCOUNT EXISTS AND CAN REACH NOTHING. Created and given a
// token but never granted, it is refused by the product REST API with a 401
// that reads exactly like a bad credential — the token is fine and there is
// nothing it may open.
const invitePath = "/admin/account-management/v1/orgs/%s/service-accounts/invite"

// lifecycleDeletePath removes an account outright.
//
// A third service: account-management creates and lists service accounts but
// cannot remove one, and user-management owns the lifecycle for every account
// including these. It takes the account id alone, with no organization.
//
// Deletion rather than deactivation, because an account that merely stops
// working still holds its seat.
const lifecycleDeletePath = "/users/%s/manage/lifecycle/delete"

// workspacesPath lists the products in an organization, and is how the site
// the agents work in is DISCOVERED rather than asked for.
//
// A POST, because Atlassian models it as a query. And v2: v3 exists and
// answers 200 with an empty list for an organization whose products are
// plainly there, so a connect built on it reports "no Jira site" against
// every real organization.
const workspacesPath = "/admin/v2/orgs/%s/workspaces"

// listPageSize is Atlassian's documented maximum for the account listing. It
// rejects anything larger with a 400 rather than clamping it.
const listPageSize = 80

// listPageLimit bounds how far a listing follows its own next links. A
// runaway guard, not a ceiling: at 80 a page it allows an organization far
// larger than any real one.
const listPageLimit = 50

// TokenLifetime is how long a minted token lasts.
//
// Atlassian caps this at 365 days. The shorter window leaves room to renew
// before an agent would otherwise go quiet.
const TokenLifetime = 300 * 24 * time.Hour

// ErrTokenNotReturned is a mint that reported success and carried no
// credential. It cannot be retried into working — the account now holds a
// token nobody has — so it is named rather than papered over.
var ErrTokenNotReturned = errors.New("atlassian: the mint returned no token")

// APIError is what Atlassian said, with the status it said it under.
type APIError struct {
	Status int
	Method string
	Path   string
	Detail string

	// RetryAfter is how long a 429 asked the call to wait, zero otherwise.
	RetryAfter time.Duration
	// Unwaited is why the call handed a 429 back rather than waiting it out.
	Unwaited string
}

func (e *APIError) Error() string {
	detail := e.Detail
	if detail == "" {
		detail = http.StatusText(e.Status)
	}
	s := fmt.Sprintf("atlassian: %s %s: %d: %s", e.Method, e.Path, e.Status, detail)
	if e.Unwaited != "" {
		s += fmt.Sprintf(" (asked to wait %s; %s)", e.RetryAfter, e.Unwaited)
	}
	return s
}

// Status is the HTTP status behind an error, or 0 when it was not one of
// Atlassian's answers. It is what the shared credential-rejection rule reads.
func Status(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// Client calls an Atlassian organization's admin APIs.
type Client struct {
	base  string
	http  *http.Client
	limit httpx.RateLimit
}

// ClientOptions configure a client.
type ClientOptions struct {
	// BaseURL overrides api.atlassian.com, for a test server.
	BaseURL string

	// HTTP overrides the transport, for a test or a caller that counts its
	// requests; nil is the vendor clients' own.
	HTTP *http.Client

	// Wait replaces the sleep a rate limit takes, for a test.
	Wait func(ctx context.Context, d time.Duration) error
}

// NewClient builds a client.
func NewClient(opts ClientOptions) *Client {
	base := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		base = AdminBaseURL
	}
	hc := opts.HTTP
	if hc == nil {
		hc = httpx.Client(ClientTimeout)
	}
	limit := httpx.VendorRateLimit()
	limit.Wait = opts.Wait
	return &Client{base: base, http: hc, limit: limit}
}

// The product keys a grant names, one per lane a seat can be on. One
// Atlassian account is one agent's identity across the site, granted each
// product its seat is on.
//
// The Jira key is `jira-software` rather than `jira`. The plain jira ARI is
// ACCEPTED when granting and silently grants nothing, which is the worst
// shape a mistake can take: a 200, an account, and no access.
const (
	GrantJira       = "jira-software"
	GrantConfluence = "confluence"
)

// PermissionRule grants one product's member role on one site.
type PermissionRule struct {
	Resource string `json:"resource"`
	Role     string `json:"role"`
}

// GrantsOf is the product access for the named products on a site, one
// member grant per product.
func GrantsOf(cloudID string, products ...string) []PermissionRule {
	out := make([]PermissionRule, 0, len(products))
	for _, product := range products {
		out = append(out, PermissionRule{
			Resource: "ari:cloud:" + product + "::site/" + cloudID,
			Role:     "ari:cloud:" + product + "::role/product/member",
		})
	}
	return out
}

// ErrAccountNotReady is a just-created account Atlassian will not grant yet.
//
// It answers a 404 whose message says the account is not in the directory,
// which reads like the account does not exist. Naming it lets the caller
// report a seat as still coming up rather than as failed.
var ErrAccountNotReady = errors.New("atlassian: the account is not grantable yet")

// Grant gives one service account access to the site's products.
func (c *Client) Grant(ctx context.Context, key, orgID, accountID string, rules []PermissionRule) error {
	if len(rules) == 0 {
		return nil
	}
	err := c.call(ctx, key, http.MethodPost, fmt.Sprintf(invitePath, orgID),
		map[string]any{"userIds": []string{accountID}, "permissionRules": rules}, nil)

	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound &&
		strings.Contains(apiErr.Detail, "not found in the directory") {
		return ErrAccountNotReady
	}
	return err
}

// ServiceAccount is one agent's identity as Atlassian holds it.
type ServiceAccount struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Email       string `json:"email"`
}

// MintedToken is a credential Atlassian issued once and will not repeat.
type MintedToken struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Token string `json:"token"`
}

// Site is the Atlassian site an organization's agents work in.
type Site struct {
	CloudID string
	HostURL string
}

// ListServiceAccounts reads every service account in the organization.
//
// EVERY one, following the collection's own next links, because stopping
// short would report an account that exists as missing and this engine would
// then create a second identity on top of a live one.
func (c *Client) ListServiceAccounts(ctx context.Context, key, orgID string) ([]ServiceAccount, error) {
	path := fmt.Sprintf(serviceAccountsPath, orgID) + "?limit=" + fmt.Sprint(listPageSize)
	out := []ServiceAccount{}

	for range listPageLimit {
		var page struct {
			Items []ServiceAccount `json:"items"`
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
		}
		if err := c.call(ctx, key, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.Links.Next == "" {
			return out, nil
		}
		// The next link is absolute. Only its path and query are kept, so
		// the call still goes through this client's base and a test server
		// is never asked to answer for api.atlassian.com.
		next, err := url.Parse(page.Links.Next)
		if err != nil {
			return nil, fmt.Errorf("atlassian: read the next page of service accounts: %w", err)
		}
		path = next.RequestURI()
	}
	return nil, fmt.Errorf(
		"atlassian: the service account listing did not end after %d pages", listPageLimit)
}

// ReadsServiceAccounts reads ONE page of the organization's service
// accounts and keeps none of it: the proof that a key can reach the
// account-management service, for a check made before the key is stored.
//
// That service rather than any other, because it is the one that refuses a
// scoped key whatever scopes it holds (see [serviceAccountsPath]): the
// workspace listing and the token routes accept a scoped key, so a check made
// against them passes the key this integration cannot create one account
// with. One page of one entry, because the answer is the status and not the
// listing.
func (c *Client) ReadsServiceAccounts(ctx context.Context, key, orgID string) error {
	path := fmt.Sprintf(serviceAccountsPath, url.PathEscape(orgID)) + "?limit=1"
	var page struct {
		Items []ServiceAccount `json:"items"`
	}
	return c.call(ctx, key, http.MethodGet, path, nil, &page)
}

// CreateServiceAccount provisions one agent's identity.
//
// The answer carries no credential; MintToken supplies that in a second call.
func (c *Client) CreateServiceAccount(
	ctx context.Context, key, orgID, displayName, description string,
) (*ServiceAccount, error) {
	account := new(ServiceAccount)
	err := c.call(ctx, key, http.MethodPost, fmt.Sprintf(serviceAccountsPath, orgID),
		map[string]any{"displayName": displayName, "description": description}, account)
	if err != nil {
		return nil, err
	}
	return account, nil
}

// JiraScopes are what a seat on the Jira lane needs: read and write work,
// and resolve the people it assigns or mentions. What the account may do in
// a project is bounded by the project's permission scheme and the seat's
// project role, not by these.
//
//nolint:gochecknoglobals // an immutable contract, not state
var JiraScopes = []string{
	"read:jira-work",
	"write:jira-work",
	"read:jira-user",
}

// ConfluenceScopes are what a seat on the Confluence lane needs: read and
// write content, read the spaces it works in, and resolve the people it
// mentions. What it may do in a space is bounded by its space role or space
// permissions.
//
//nolint:gochecknoglobals // an immutable contract, not state
var ConfluenceScopes = []string{
	"read:confluence-content.all",
	"write:confluence-content",
	"read:confluence-space.summary",
	"read:confluence-user",
}

// ScopesFor is the union of scopes for the lanes a seat is on. One token
// carries both products, because one Atlassian account is one agent's
// identity across the site; a lane that is off contributes nothing, so its
// product refuses the token even while the account keeps its product grant.
func ScopesFor(jira, confluence bool) []string {
	var out []string
	if jira {
		out = append(out, JiraScopes...)
	}
	if confluence {
		out = append(out, ConfluenceScopes...)
	}
	return out
}

// MintToken issues an API token on one service account.
//
// The label is STAMPED with the mint time. Labels are unique within an
// account and an account outlives any one token, so a second mint would
// otherwise collide with the first.
//
// Scopes are REQUIRED: a mint with none answers 400 MISSING_SCOPES. An empty
// set is refused here, before the call, because it means the seat is on no
// lane and should hold no token at all.
func (c *Client) MintToken(
	ctx context.Context, key, accountID, label string, scopes []string, now time.Time,
) (*MintedToken, error) {
	if len(scopes) == 0 {
		return nil, errors.New("atlassian: a token needs at least one scope; " +
			"a seat on no lane holds no token")
	}
	body := map[string]any{
		"label":  fmt.Sprintf("%s-%d", label, now.Unix()),
		"scopes": scopes,
		"expiry": now.Add(TokenLifetime).Format(time.RFC3339),
	}
	token := new(MintedToken)
	if err := c.call(ctx, key, http.MethodPost,
		fmt.Sprintf(apiTokensPath, accountID), body, token); err != nil {
		return nil, err
	}
	if token.Token == "" {
		return nil, ErrTokenNotReturned
	}
	return token, nil
}

// APIToken is one token an account holds, as Atlassian lists it: never the
// value, which is shown once at mint.
// Source: https://developer.atlassian.com/cloud/admin/user-management/rest/api-group-api-tokens/
type APIToken struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	CreatedAt  string `json:"createdAt"`
	LastAccess string `json:"lastAccess"`
	Expiry     string `json:"expiry"`
}

// ListTokens reads the API tokens one account holds.
func (c *Client) ListTokens(ctx context.Context, key, accountID string) ([]APIToken, error) {
	var out []APIToken
	if err := c.call(ctx, key, http.MethodGet,
		fmt.Sprintf(apiTokensPath, url.PathEscape(accountID)), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RevokeToken deletes one API token. A token already gone is the state asked
// for, so a 404 is success.
func (c *Client) RevokeToken(ctx context.Context, key, accountID, tokenID string) error {
	err := c.call(ctx, key, http.MethodDelete,
		fmt.Sprintf(apiTokensPath, url.PathEscape(accountID))+"/"+url.PathEscape(tokenID), nil, nil)
	if Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// DeleteServiceAccount removes an agent's identity.
func (c *Client) DeleteServiceAccount(ctx context.Context, key, accountID string) error {
	return c.call(ctx, key, http.MethodPost, fmt.Sprintf(lifecycleDeletePath, accountID),
		map[string]any{"message": "removed by Crewlet"}, nil)
}

// DiscoverSite finds the Atlassian site the organization's agents work in.
//
// DISCOVERED rather than asked for: an organization knows its own products,
// and a site address typed into a form is one more thing to get wrong. A
// Jira workspace is preferred over a Confluence one only for the host it
// carries; both name the same cloud id on a single-site organization.
func (c *Client) DiscoverSite(ctx context.Context, key, orgID string) (*Site, error) {
	workspaces, err := c.Workspaces(ctx, key, orgID)
	if err != nil {
		return nil, err
	}
	site := &Site{}
	for _, w := range workspaces {
		cloudID := w.CloudID()
		if cloudID == "" {
			continue
		}
		if site.CloudID == "" || strings.HasPrefix(w.Type, "Jira") {
			site.CloudID, site.HostURL = cloudID, w.SiteURL()
		}
	}
	if site.CloudID == "" {
		return nil, errors.New(
			"atlassian: this organization lists no Jira or Confluence site, so there " +
				"is nowhere for an agent's account to work")
	}
	return site, nil
}

// Workspace is one product of an organization, as the listing answers it:
// its identifier, which product it is, and the address it is reached at.
// CLOSED FIELDS: a row also carries who created the workspace and who owns
// it, which nothing here decodes.
type Workspace struct {
	// ID is the workspace's ARI, `ari:cloud:<product>::site/<cloud id>` for
	// a product on a site.
	ID string
	// TypeKey is the product's key (`jira-software`, `confluence`), and Type
	// its display name.
	TypeKey string
	Type    string
	HostURL string
}

// CloudID is the id of the site the workspace is on, "" for a product that
// sits on no site.
func (w Workspace) CloudID() string { return cloudIDFromARI(w.ID) }

// SiteURL is the address of the site the workspace is on. A Confluence host
// carries the /wiki suffix; the site is the address underneath it, and each
// product's own address is derived from that rather than stored twice.
func (w Workspace) SiteURL() string {
	return strings.TrimSuffix(strings.TrimRight(w.HostURL, "/"), "/wiki")
}

// Product is the product's key: the listing's own, or the one the ARI
// names where an older row carries none.
func (w Workspace) Product() string {
	if key := strings.ToLower(strings.TrimSpace(w.TypeKey)); key != "" {
		return key
	}
	rest, ok := strings.CutPrefix(w.ID, "ari:cloud:")
	if !ok {
		return ""
	}
	product, _, _ := strings.Cut(rest, ":")
	return strings.ToLower(product)
}

// Workspaces is every product of the organization, to the listing's end.
//
// EVERY PAGE, following the listing's own cursor, because the site the
// agents work in can sit past the first page of an organization with many
// products, and a caller that reads the organization whole would otherwise
// take what it did not reach for gone.
func (c *Client) Workspaces(ctx context.Context, key, orgID string) ([]Workspace, error) {
	path := fmt.Sprintf(workspacesPath, url.PathEscape(orgID))
	var out []Workspace
	cursor := ""
	for range listPageLimit {
		body := map[string]any{}
		if cursor != "" {
			body["cursor"] = cursor
		}
		var page struct {
			Data []struct {
				ID         string `json:"id"`
				Attributes struct {
					TypeKey string `json:"typeKey"`
					Type    string `json:"type"`
					HostURL string `json:"hostUrl"`
				} `json:"attributes"`
			} `json:"data"`
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
		}
		if err := c.call(ctx, key, http.MethodPost, path, body, &page); err != nil {
			return nil, err
		}
		for _, w := range page.Data {
			out = append(out, Workspace{ID: w.ID, TypeKey: w.Attributes.TypeKey, Type: w.Attributes.Type,
				HostURL: w.Attributes.HostURL})
		}
		next, err := cursorOf(page.Links.Next)
		if err != nil {
			return nil, err
		}
		if next == "" {
			return out, nil
		}
		if next == cursor {
			return nil, errors.New("atlassian: the workspace listing answered the page it was asked for as its next")
		}
		cursor = next
	}
	return nil, fmt.Errorf("atlassian: the workspace listing did not finish after %d pages", listPageLimit)
}

// cursorOf is the cursor a listing's next link carries, "" where there is no
// next page. The listing takes its cursor in the request's body, and its
// link is the cursor itself or an address carrying it as `cursor`: either is
// read, and a link that is neither is an error, since a listing cut short
// there would read as a whole one.
func cursorOf(next string) (string, error) {
	next = strings.TrimSpace(next)
	if next == "" {
		return "", nil
	}
	if !strings.Contains(next, "/") && !strings.Contains(next, "?") {
		return next, nil
	}
	link, err := url.Parse(next)
	if err != nil || link.Query().Get("cursor") == "" {
		return "", errors.New("atlassian: the workspace listing's next link carries no cursor")
	}
	return link.Query().Get("cursor"), nil
}

// cloudIDFromARI pulls the site id out of `ari:cloud:jira::site/<uuid>`.
func cloudIDFromARI(ari string) string {
	_, id, found := strings.Cut(ari, "site/")
	if !found {
		return ""
	}
	return strings.TrimSpace(id)
}

// call performs one request and decodes its answer, waiting out a short 429
// as Atlassian asks. A 429 is the rate limiter refusing a request before it
// is processed, so sending it again repeats nothing, whatever its method.
func (c *Client) call(ctx context.Context, key, method, path string, body, out any) error {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return fmt.Errorf("atlassian: encode %s %s: %w", method, path, err)
		}
	}
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, key, method, path, encoded, out)
		api, ok := errors.AsType[*APIError](err)
		if !ok || api.Status != http.StatusTooManyRequests {
			return err
		}
		unwaited := c.limit.WaitOut(ctx, attempt, api.RetryAfter)
		if unwaited == nil {
			continue
		}
		api.Unwaited = unwaited.Error()
		return api
	}
}

// once sends one request and decodes its answer. It is the only place a
// request is built.
func (c *Client) once(ctx context.Context, key, method, path string, body []byte, out any) error {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, payload)
	if err != nil {
		return fmt.Errorf("atlassian: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("atlassian: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("atlassian: read %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		api := &APIError{
			Status: resp.StatusCode, Method: method, Path: path,
			Detail: detailFrom(resp.Header.Get("Content-Type"), answer),
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			wait, named := httpx.RetryAfter(resp.Header, time.Now())
			if !named {
				wait = httpx.DefaultRetryAfter
			}
			api.RetryAfter = wait
		}
		return api
	}
	if out == nil || len(answer) == 0 {
		return nil
	}
	if err := json.Unmarshal(answer, out); err != nil {
		return fmt.Errorf("atlassian: decode %s %s: %w", method, path, err)
	}
	return nil
}

// detailFrom pulls Atlassian's own words out of a refusal.
//
// Its admin APIs answer in more than one shape, so this tries each — and then
// hands anything else to [httpx.Refusal] rather than to the caller verbatim.
//
// IT USED TO RETURN THE RAW BODY, on the reasoning that an operator needs what
// Atlassian said and an empty string helps nobody. Both clauses are true and
// the conclusion was wrong for the shape Atlassian actually sends: a 403 from
// its admin API arrives as an HTML page, so a whole rendered document —
// doctype, head, inline styles, script tags — reached the log around a
// sentence nobody could find. The body is read with a megabyte cap, so it
// reached it in full.
func detailFrom(contentType string, body []byte) string {
	var shaped struct {
		Message string `json:"message"`
		Detail  string `json:"detail"`
		Errors  []struct {
			Detail string `json:"detail"`
			Title  string `json:"title"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &shaped); err == nil {
		for _, candidate := range []string{shaped.Message, shaped.Detail} {
			if candidate != "" {
				return candidate
			}
		}
		for _, e := range shaped.Errors {
			if e.Detail != "" {
				return e.Detail
			}
			if e.Title != "" {
				return e.Title
			}
		}
	}
	return httpx.Refusal(contentType, body)
}
