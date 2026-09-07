package datadog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// ClientTimeout bounds one call to Datadog.
//
// Datadog's own documented p99 for the v2 API is under two seconds, and every
// call here is made inside a reconcile pass that already has a deadline of
// its own. Ten seconds is slack for a slow region, not a budget to spend.
const ClientTimeout = 10 * time.Second

// Datadog's regions.
//
// A key issued in one region is refused by every other, and the hostname is
// the only thing that distinguishes them — so a company's site is CHECKED
// against this list rather than trusted. A typo would otherwise be a
// credential that authenticates nowhere, reported as a rejected key.
//
//nolint:gochecknoglobals // an immutable list, not state
var sites = []string{
	"datadoghq.com",
	"us3.datadoghq.com",
	"us5.datadoghq.com",
	"datadoghq.eu",
	"ap1.datadoghq.com",
	"ap2.datadoghq.com",
	"ddog-gov.com",
}

// Sites is every region this build serves, in the order the setup form
// offers them.
func Sites() []string { return slices.Clone(sites) }

// KnownSite reports whether a site is one Datadog actually serves.
func KnownSite(site string) bool { return slices.Contains(sites, normalizeSite(site)) }

func normalizeSite(site string) string {
	return strings.TrimSpace(strings.ToLower(site))
}

// Credentials are the pair every Datadog call carries.
//
// TWO KEYS, NOT ONE, and they are not interchangeable: the API key says which
// organization, and the APPLICATION key says which user acts. Datadog refuses
// a v2 write carrying only the first, with a message that names neither.
type Credentials struct {
	APIKey string
	AppKey string
}

// APIError is a call Datadog refused.
type APIError struct {
	Method string
	Path   string
	Status int
	Detail string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("datadog: %s %s: %d: %s", e.Method, e.Path, e.Status, e.Detail)
}

// Status reports the HTTP status a call was refused with, or 0 when the
// failure was not an API error.
//
// The same accessor every other third-party app client exports, for the same
// reason: a caller deciding what a refusal MEANS needs the number, and the
// meaning is decided once, in [integration.Reject], rather than per app.
func Status(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	return 0
}

// Client talks to one Datadog region.
type Client struct {
	site string
	http *http.Client
}

// ClientOptions builds a client.
type ClientOptions struct {
	// Site is the region hostname, e.g. datadoghq.eu.
	Site string
	// HTTP overrides the transport, for tests.
	HTTP *http.Client
}

// NewClient builds a client for one region.
//
// It REFUSES an unknown site rather than accepting one: every later call is
// built from this hostname, so a typo becomes a credential that authenticates
// nowhere and reports as a rejected key.
func NewClient(opts ClientOptions) (*Client, error) {
	site := normalizeSite(opts.Site)
	if site == "" {
		return nil, errors.New("datadog: no site")
	}
	if !KnownSite(site) {
		return nil, fmt.Errorf(
			"datadog: %q is not a region Datadog serves — it is one of %s",
			opts.Site, strings.Join(sites, ", "))
	}
	client := opts.HTTP
	if client == nil {
		client = httpx.Client(ClientTimeout)
	}
	return &Client{site: site, http: client}, nil
}

// Site is the region this client talks to.
func (c *Client) Site() string { return c.site }

func (c *Client) endpoint(path string) string {
	return "https://api." + c.site + path
}

// do makes one call, and is the only place a Datadog request is built.
func (c *Client) do(
	ctx context.Context, method, path string, creds Credentials, body, out any,
) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("datadog: encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), reader)
	if err != nil {
		return fmt.Errorf("datadog: build %s %s: %w", method, path, err)
	}
	req.Header.Set("DD-API-KEY", creds.APIKey)
	req.Header.Set("DD-APPLICATION-KEY", creds.AppKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("datadog: %s %s: %w", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("datadog: read %s %s: %w", method, path, err)
	}
	if res.StatusCode >= 300 {
		return &APIError{
			Method: method, Path: path, Status: res.StatusCode,
			Detail: detailOf(payload),
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("datadog: decode %s %s: %w", method, path, err)
	}
	return nil
}

// maxResponse bounds what one answer may be.
//
// A list page is a few hundred accounts at most and every call here reads
// one. The cap is what stops a proxy's HTML error page, or a region
// answering something unexpected, from being read into memory whole.
const maxResponse = 4 << 20

// detailOf pulls Datadog's own message out of a refusal, so an operator
// reads what Datadog said rather than a status code.
func detailOf(payload []byte) string {
	var body struct {
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(payload, &body); err == nil && len(body.Errors) > 0 {
		return strings.Join(body.Errors, "; ")
	}
	detail := strings.TrimSpace(string(payload))
	if detail == "" {
		return "no detail"
	}
	return detail
}

// Org is the organization a credential pair belongs to.
type Org struct {
	Name   string
	Public string
}

// VerifyCredentials reads the organization the keys authenticate as.
//
// THE CONNECT-TIME CHECK, and it earns its call: a key pair that is wrong is
// otherwise discovered by the first provisioning pass, minutes later, as a
// refusal on a screen nobody is watching. It also names the ORGANIZATION,
// which is the one thing an operator can check to see they connected the
// account they meant to.
func (c *Client) VerifyCredentials(ctx context.Context, creds Credentials) (Org, error) {
	var body struct {
		Data []struct {
			Attributes struct {
				Name       string `json:"name"`
				PublicID   string `json:"public_id"`
				Disabled   bool   `json:"disabled"`
				Sharing    string `json:"sharing"`
				Descriptio string `json:"description"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v2/current_user/orgs", creds, nil, &body); err != nil {
		return Org{}, err
	}
	if len(body.Data) == 0 {
		return Org{}, errors.New(
			"datadog: the keys authenticated and named no organization, which " +
				"is an answer this build cannot act on")
	}
	first := body.Data[0].Attributes
	return Org{Name: first.Name, Public: first.PublicID}, nil
}

// User is a Datadog user or service account.
type User struct {
	ID       string
	Email    string
	Name     string
	Disabled bool
}

// ListServiceAccounts reads the service accounts whose email matches query.
//
// FILTERED AT THE VENDOR rather than here: an organization may hold thousands
// of users, and walking all of them to find the handful this engine made
// spends a rate limit on rows nobody reads.
func (c *Client) ListServiceAccounts(ctx context.Context, creds Credentials, query string) ([]User, error) {
	path := "/api/v2/users?filter=" + url.QueryEscape(query) +
		"&page[size]=" + fmt.Sprint(listPageSize)
	var body struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				Email      string `json:"email"`
				Name       string `json:"name"`
				Disabled   bool   `json:"disabled"`
				ServiceAcc bool   `json:"service_account"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, path, creds, nil, &body); err != nil {
		return nil, err
	}
	out := make([]User, 0, len(body.Data))
	for _, row := range body.Data {
		if !row.Attributes.ServiceAcc {
			// A PERSON matched the filter. Returning them would let a
			// caller looking for its own accounts adopt somebody's real
			// user and then disable it.
			continue
		}
		out = append(out, User{
			ID: row.ID, Email: row.Attributes.Email,
			Name: row.Attributes.Name, Disabled: row.Attributes.Disabled,
		})
	}
	return out, nil
}

// listPageSize is what one listing asks for. Datadog's documented maximum is
// 100; a larger value is clamped server-side, which would make a partial walk
// look like a complete one.
const listPageSize = 100

// Role is a Datadog role, which is how permission is granted.
type Role struct {
	ID   string
	Name string
}

// ListRoles reads the roles whose name matches query.
func (c *Client) ListRoles(ctx context.Context, creds Credentials, query string) ([]Role, error) {
	path := "/api/v2/roles?filter=" + url.QueryEscape(query) +
		"&page[size]=" + fmt.Sprint(listPageSize)
	var body struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				Name string `json:"name"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, path, creds, nil, &body); err != nil {
		return nil, err
	}
	out := make([]Role, 0, len(body.Data))
	for _, row := range body.Data {
		out = append(out, Role{ID: row.ID, Name: row.Attributes.Name})
	}
	return out, nil
}

// CreateServiceAccount provisions one agent's identity ALREADY HOLDING its
// role.
//
// The role is passed at creation rather than granted after, and that is not
// tidiness: an account that exists for a moment without one inherits the
// organization's default role, and a pass interrupted between the two calls
// leaves an agent holding whatever that default grants. Datadog accepts both
// in one request, so there is no window to be caught in.
func (c *Client) CreateServiceAccount(
	ctx context.Context, creds Credentials, email, name, roleID string,
) (User, error) {
	attributes := map[string]any{
		"email":           email,
		"name":            name,
		"service_account": true,
	}
	payload := map[string]any{
		"data": map[string]any{
			"type":       "users",
			"attributes": attributes,
			"relationships": map[string]any{
				"roles": map[string]any{
					"data": []map[string]string{{"id": roleID, "type": "roles"}},
				},
			},
		},
	}
	var body struct {
		Data struct {
			ID         string `json:"id"`
			Attributes struct {
				Email string `json:"email"`
				Name  string `json:"name"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v2/service_accounts", creds, payload, &body); err != nil {
		return User{}, err
	}
	return User{
		ID: body.Data.ID, Email: body.Data.Attributes.Email, Name: body.Data.Attributes.Name,
	}, nil
}

// AppKey is an application key belonging to a service account.
type AppKey struct {
	ID   string
	Name string
	// Key is the secret, and Datadog returns it EXACTLY ONCE, on creation.
	// Every later read answers with the id and the name only.
	Key string
}

// CreateAppKey mints the key one agent authenticates with.
//
// THE VALUE IS RETURNED ONCE. Datadog never shows it again, so a pass that
// creates a key and then fails to record it has stranded a credential that
// can only be deleted and remade. Every caller here writes it to the sink
// before doing anything else.
func (c *Client) CreateAppKey(
	ctx context.Context, creds Credentials, accountID, name string,
) (AppKey, error) {
	payload := map[string]any{
		"data": map[string]any{
			"type":       "application_keys",
			"attributes": map[string]any{"name": name},
		},
	}
	path := "/api/v2/service_accounts/" + url.PathEscape(accountID) + "/application_keys"
	var body struct {
		Data struct {
			ID         string `json:"id"`
			Attributes struct {
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, path, creds, payload, &body); err != nil {
		return AppKey{}, err
	}
	return AppKey{
		ID: body.Data.ID, Name: body.Data.Attributes.Name, Key: body.Data.Attributes.Key,
	}, nil
}

// ListAppKeys reads one account's keys, without their values.
func (c *Client) ListAppKeys(ctx context.Context, creds Credentials, accountID string) ([]AppKey, error) {
	path := "/api/v2/service_accounts/" + url.PathEscape(accountID) + "/application_keys"
	var body struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				Name string `json:"name"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, path, creds, nil, &body); err != nil {
		return nil, err
	}
	out := make([]AppKey, 0, len(body.Data))
	for _, row := range body.Data {
		out = append(out, AppKey{ID: row.ID, Name: row.Attributes.Name})
	}
	return out, nil
}

// DisableUser deactivates a service account.
//
// DISABLED, NOT DELETED, and Datadog's own API is why: deleting a user
// detaches it from everything it did, so dashboards, monitors and notebooks
// it authored lose their author. A disabled service account keeps what it
// made and can do nothing more, which is what decommissioning one means.
func (c *Client) DisableUser(ctx context.Context, creds Credentials, userID string) error {
	return c.do(ctx, http.MethodDelete, "/api/v2/users/"+url.PathEscape(userID), creds, nil, nil)
}
