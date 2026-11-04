package scaleway

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
)

// APIBase is Scaleway's API. Fixed rather than configurable, so a provisioner
// key is only ever sent to Scaleway.
const APIBase = "https://api.scaleway.com"

// ClientTimeout bounds one call. Every call runs inside a pass with its own
// deadline, so this is slack for a slow answer rather than a budget to spend.
const ClientTimeout = 15 * time.Second

// Rate limiting: Scaleway answers a 429 with `Retry-After`. A wait up to
// rateLimitWaitCap is taken in the call, up to rateLimitRetries times; a
// longer one is returned to the pass, whose loop backs off on its own.
const (
	rateLimitRetries  = 3
	rateLimitWaitCap  = 30 * time.Second
	defaultRetryAfter = 5 * time.Second
)

// maxResponse bounds one answer: a page of 100 permission sets is tens of
// kilobytes, and the cap stops a proxy's page being read whole.
const maxResponse = 4 << 20

// Paging: listings ask for pageSize rows, the most Scaleway serves, and stop
// after pageCeiling pages (10,000 rows) with an error, since a short read
// would read as rows that do not exist.
const (
	pageSize    = 100
	pageCeiling = 100
)

// The shapes Scaleway gives an organization, project and key identifier.
// Compiled at first use, not at init: the code host's push hook is this
// binary and pays every package's init (cmd/crewlet's hookinit_test.go),
// and a bounded repeat compiles to a program holding every count.
// The two of them were this package's whole init, about 26 KB.
var (
	uuidPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	})
	accessKeyPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^SCW[A-Z0-9]{17}$`)
	})
)

// APIError is a call Scaleway refused. It holds what Scaleway said, which
// never includes a credential.
type APIError struct {
	Method string
	Path   string
	Status int

	// Type is Scaleway's error type (`permissions_denied`,
	// `quotas_exceeded`, `invalid_arguments`), and Detail its message.
	Type   string
	Detail string

	// RetryAfter is Scaleway's own wait on a 429, zero otherwise.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("scaleway: %s %s: %d", e.Method, e.Path, e.Status)
	if e.Type != "" {
		s += " " + e.Type
	}
	return s + ": " + e.Detail
}

// Status reports the HTTP status a call was refused with, or 0 when the
// failure was not an API error, so [integration.Reject] can classify it.
func Status(err error) int {
	if api, ok := errors.AsType[*APIError](err); ok {
		return api.Status
	}
	return 0
}

// errorType is Scaleway's error type for a refused call, "" otherwise.
func errorType(err error) string {
	if api, ok := errors.AsType[*APIError](err); ok {
		return api.Type
	}
	return ""
}

// QuotaExceeded reports a refusal because the organization holds as many of a
// resource as its quota allows.
func QuotaExceeded(err error) bool { return errorType(err) == "quotas_exceeded" }

// ExpiryRefused reports a key refused for its expiry: an organization whose
// security settings cap API keys' lifetime below the one asked for.
func ExpiryRefused(err error) bool {
	api, ok := errors.AsType[*APIError](err)
	return ok && api.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(api.Detail), "expir")
}

// reject is [integration.Reject] for a Scaleway error.
func reject(err error) error { return integration.Reject(err, Status(err)) }

// noDetail is what a refusal that said nothing reads as.
const noDetail = "no detail"

// Client talks to one Scaleway organization with one key.
type Client struct {
	org    string
	secret string
	http   *http.Client
	wait   func(ctx context.Context, d time.Duration) error
}

// ClientOptions builds a client.
type ClientOptions struct {
	// OrganizationID is the organization every listing and every created
	// resource names.
	OrganizationID string
	// SecretKey is the API key's secret every call carries.
	SecretKey string
	// HTTP overrides the transport, for tests.
	HTTP *http.Client
}

// NewClient builds a client, refusing an organization ID that is not one and
// an empty secret, either of which would fail every call with a misleading
// 4xx.
func NewClient(opts ClientOptions) (*Client, error) {
	org := config.ScalewayID(opts.OrganizationID)
	if !uuidPattern().MatchString(org) {
		return nil, errors.New("scaleway: the Organization ID is not a UUID")
	}
	secret := strings.TrimSpace(opts.SecretKey)
	if secret == "" {
		return nil, errors.New("scaleway: no secret key")
	}
	hc := opts.HTTP
	if hc == nil {
		hc = httpx.Client(ClientTimeout)
	}
	return &Client{org: org, secret: secret, http: hc, wait: sleep}, nil
}

// OrganizationID is the organization this client addresses.
func (c *Client) OrganizationID() string { return c.org }

// The console pages where a person settles what only Scaleway can.
const (
	ApplicationsURL = "https://console.scaleway.com/iam/applications"
	PoliciesURL     = "https://console.scaleway.com/iam/policies"
	APIKeysURL      = "https://console.scaleway.com/iam/api-keys"
	ProjectsURL     = "https://console.scaleway.com/organization/projects"
	OrganizationURL = "https://console.scaleway.com/organization/settings"
	SecurityURL     = "https://console.scaleway.com/settings/organization/security"
	QuotasURL       = "https://console.scaleway.com/organization/quotas"
)

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// do makes one call and decodes its answer into out, waiting out a short 429
// as Scaleway asks. It is the only place a request is built.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return fmt.Errorf("scaleway: encode %s %s: %w", method, path, err)
		}
	}
	for attempt := 0; ; attempt++ {
		payload, err := c.once(ctx, method, path, query, encoded)
		var api *APIError
		if errors.As(err, &api) && api.Status == http.StatusTooManyRequests &&
			attempt < rateLimitRetries && api.RetryAfter <= rateLimitWaitCap && fits(ctx, api.RetryAfter) {
			if werr := c.wait(ctx, api.RetryAfter); werr != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if out != nil && len(bytes.TrimSpace(payload)) > 0 {
			if err := json.Unmarshal(payload, out); err != nil {
				return fmt.Errorf("scaleway: decode %s %s: %w", method, path, err)
			}
		}
		return nil
	}
}

// fits reports whether a wait ends before ctx's deadline.
func fits(ctx context.Context, d time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) > d
}

// once sends one request and reads its answer.
func (c *Client) once(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
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
		return nil, fmt.Errorf("scaleway: build %s %s: %w", method, path, err)
	}
	req.Header.Set("X-Auth-Token", c.secret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scaleway: %s %s: %w", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return nil, fmt.Errorf("scaleway: read %s %s: %w", method, path, err)
	}
	if res.StatusCode < http.StatusMultipleChoices {
		return payload, nil
	}
	api := &APIError{Method: method, Path: path, Status: res.StatusCode}
	var refusal struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	}
	if json.Unmarshal(payload, &refusal) == nil {
		api.Type = refusal.Type
		said := strings.TrimSpace(refusal.Message)
		if d := strings.TrimSpace(string(refusal.Details)); d != "" && d != "null" && d != "[]" {
			said = strings.TrimSpace(said + " " + d)
		}
		api.Detail = httpx.Refusal("text/plain", []byte(said))
	}
	if api.Detail == "" {
		api.Detail = httpx.Refusal(res.Header.Get("Content-Type"), payload)
	}
	if api.Detail == "" {
		api.Detail = noDetail
	}
	if res.StatusCode == http.StatusTooManyRequests {
		api.RetryAfter = retryAfter(res.Header.Get("Retry-After"))
	}
	return nil, api
}

// retryAfter reads Scaleway's wait in seconds, or the default where it named
// none.
func retryAfter(raw string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultRetryAfter
}

// list reads a paged collection to exhaustion: each page's rows sit under
// field, beside the collection's total_count.
func list[T any](ctx context.Context, c *Client, path string, query url.Values, field string) ([]T, error) {
	var out []T
	for page := 1; ; page++ {
		if page > pageCeiling {
			return nil, fmt.Errorf("scaleway: %s did not finish after %d pages of %d", path, pageCeiling, pageSize)
		}
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("page", strconv.Itoa(page))
		q.Set("page_size", strconv.Itoa(pageSize))
		var raw map[string]json.RawMessage
		if err := c.do(ctx, http.MethodGet, path, q, nil, &raw); err != nil {
			return nil, err
		}
		var rows []T
		if r, ok := raw[field]; ok {
			if err := json.Unmarshal(r, &rows); err != nil {
				return nil, fmt.Errorf("scaleway: decode %s: %w", path, err)
			}
		}
		var total uint64
		if r, ok := raw["total_count"]; ok {
			if err := json.Unmarshal(r, &total); err != nil {
				return nil, fmt.Errorf("scaleway: decode %s's total: %w", path, err)
			}
		}
		out = append(out, rows...)
		if len(rows) == 0 || uint64(len(out)) >= total {
			return out, nil
		}
	}
}

// gone reports a delete or a read of something Scaleway no longer has.
func gone(err error) bool { return Status(err) == http.StatusNotFound }

// APIKey is one API key, as Scaleway reports it. SecretKey is set only in the
// answer that created it.
type APIKey struct {
	AccessKey        string     `json:"access_key"`
	SecretKey        string     `json:"secret_key"`
	ApplicationID    string     `json:"application_id"`
	UserID           string     `json:"user_id"`
	Description      string     `json:"description"`
	DefaultProjectID string     `json:"default_project_id"`
	CreatedAt        *time.Time `json:"created_at"`
	ExpiresAt        *time.Time `json:"expires_at"`
}

// Expires is when the key stops working, zero for one that never does.
func (k APIKey) Expires() time.Time {
	if k.ExpiresAt == nil {
		return time.Time{}
	}
	return k.ExpiresAt.UTC()
}

// Application is one IAM application.
type Application struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Description    string     `json:"description"`
	OrganizationID string     `json:"organization_id"`
	CreatedAt      *time.Time `json:"created_at"`
}

// Policy is one IAM policy.
type Policy struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	ApplicationID string     `json:"application_id"`
	CreatedAt     *time.Time `json:"created_at"`
}

// Rule is one rule of a policy: permission sets on projects, or on the
// organization, under an optional condition.
type Rule struct {
	PermissionSetNames []string `json:"permission_set_names"`
	ProjectIDs         []string `json:"project_ids"`
	OrganizationID     *string  `json:"organization_id"`
	Condition          string   `json:"condition"`
}

// OnOrganization reports a rule scoped to the organization rather than to
// projects.
func (r Rule) OnOrganization() bool { return r.OrganizationID != nil && *r.OrganizationID != "" }

// RuleSpec is one rule as a policy is written with it: ProjectIDs or
// OrganizationID, never both.
type RuleSpec struct {
	PermissionSetNames []string `json:"permission_set_names"`
	ProjectIDs         []string `json:"project_ids,omitempty"`
	OrganizationID     string   `json:"organization_id,omitempty"`
}

// PermissionSet is one entry of the organization's catalogue.
type PermissionSet struct {
	Name      string `json:"name"`
	ScopeType string `json:"scope_type"`
}

// Project is one project of the organization.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// APIKey reads one key by its access key, and false where Scaleway has none.
func (c *Client) APIKey(ctx context.Context, access string) (APIKey, bool, error) {
	var key APIKey
	err := c.do(ctx, http.MethodGet, "/iam/v1alpha1/api-keys/"+url.PathEscape(access), nil, nil, &key)
	switch {
	case gone(err):
		return APIKey{}, false, nil
	case err != nil:
		return APIKey{}, false, err
	}
	return key, true, nil
}

// OwnKey reads the key this client's secret belongs to, by its access key,
// else, where Scaleway refuses that read, through the listing filtered to it.
// direct is false where the read by access key was refused and the listing
// answered instead, which is the mark of a condition refusing key operations
// on the provisioner's own rule.
func (c *Client) OwnKey(ctx context.Context, access string) (key APIKey, direct bool, err error) {
	key, found, err := c.APIKey(ctx, access)
	switch {
	case err == nil && found:
		return key, true, nil
	case err == nil:
		return APIKey{}, false, &APIError{Method: http.MethodGet, Path: "/iam/v1alpha1/api-keys/" + access,
			Status: http.StatusNotFound, Type: "not_found", Detail: "Scaleway has no API key with this access key"}
	case Status(err) != http.StatusForbidden:
		return APIKey{}, false, err
	}
	keys, lerr := list[APIKey](ctx, c, "/iam/v1alpha1/api-keys",
		url.Values{"organization_id": {c.org}, "access_keys": {access}}, "api_keys")
	if lerr != nil {
		return APIKey{}, false, err
	}
	for _, k := range keys {
		if k.AccessKey == access {
			return k, false, nil
		}
	}
	return APIKey{}, false, err
}

// APIKeysOf is every API key whose bearer is the application.
func (c *Client) APIKeysOf(ctx context.Context, applicationID string) ([]APIKey, error) {
	return list[APIKey](ctx, c, "/iam/v1alpha1/api-keys", url.Values{
		"organization_id": {c.org}, "bearer_id": {applicationID}, "bearer_type": {"application"},
	}, "api_keys")
}

// KeySpec is an API key as one is created for an application.
type KeySpec struct {
	ApplicationID    string
	Description      string
	DefaultProjectID string
	ExpiresAt        time.Time
}

// CreateAPIKey creates a key and answers it with its secret, which Scaleway
// shows only here.
func (c *Client) CreateAPIKey(ctx context.Context, spec KeySpec) (APIKey, error) {
	body := map[string]any{"application_id": spec.ApplicationID, "description": spec.Description,
		"expires_at": spec.ExpiresAt.UTC().Format(time.RFC3339)}
	if spec.DefaultProjectID != "" {
		body["default_project_id"] = spec.DefaultProjectID
	}
	var key APIKey
	if err := c.do(ctx, http.MethodPost, "/iam/v1alpha1/api-keys", nil, body, &key); err != nil {
		return APIKey{}, err
	}
	return key, nil
}

// SetDefaultProject moves a key's default project, the one Object Storage acts
// in.
func (c *Client) SetDefaultProject(ctx context.Context, access, projectID string) error {
	return c.do(ctx, http.MethodPatch, "/iam/v1alpha1/api-keys/"+url.PathEscape(access), nil,
		map[string]any{"default_project_id": projectID}, nil)
}

// DeleteAPIKey deletes a key; one already gone is the state asked for.
func (c *Client) DeleteAPIKey(ctx context.Context, access string) error {
	if err := c.do(ctx, http.MethodDelete, "/iam/v1alpha1/api-keys/"+url.PathEscape(access), nil, nil, nil); err != nil && !gone(err) {
		return err
	}
	return nil
}

// Application reads one application, and false where Scaleway has none.
func (c *Client) Application(ctx context.Context, id string) (Application, bool, error) {
	var app Application
	err := c.do(ctx, http.MethodGet, "/iam/v1alpha1/applications/"+url.PathEscape(id), nil, nil, &app)
	switch {
	case gone(err):
		return Application{}, false, nil
	case err != nil:
		return Application{}, false, err
	}
	return app, true, nil
}

// Applications is every application of the organization.
func (c *Client) Applications(ctx context.Context) ([]Application, error) {
	return list[Application](ctx, c, "/iam/v1alpha1/applications",
		url.Values{"organization_id": {c.org}}, "applications")
}

// CreateApplication creates an application in the organization.
func (c *Client) CreateApplication(ctx context.Context, name, description string) (Application, error) {
	var app Application
	err := c.do(ctx, http.MethodPost, "/iam/v1alpha1/applications", nil, map[string]any{
		"name": name, "organization_id": c.org, "description": description,
	}, &app)
	return app, err
}

// DeleteApplication deletes an application; one already gone is the state
// asked for.
func (c *Client) DeleteApplication(ctx context.Context, id string) error {
	if err := c.do(ctx, http.MethodDelete, "/iam/v1alpha1/applications/"+url.PathEscape(id), nil, nil, nil); err != nil && !gone(err) {
		return err
	}
	return nil
}

// PoliciesOf is every policy whose principal is the application.
func (c *Client) PoliciesOf(ctx context.Context, applicationID string) ([]Policy, error) {
	return list[Policy](ctx, c, "/iam/v1alpha1/policies", url.Values{
		"organization_id": {c.org}, "application_ids": {applicationID},
	}, "policies")
}

// PolicySpec is a policy as one is created for an application.
type PolicySpec struct {
	Name          string
	Description   string
	ApplicationID string
	Rules         []RuleSpec
}

// CreatePolicy creates a policy with its rules.
func (c *Client) CreatePolicy(ctx context.Context, spec PolicySpec) (Policy, error) {
	var pol Policy
	err := c.do(ctx, http.MethodPost, "/iam/v1alpha1/policies", nil, map[string]any{
		"name": spec.Name, "description": spec.Description, "organization_id": c.org,
		"application_id": spec.ApplicationID, "rules": spec.Rules,
	}, &pol)
	return pol, err
}

// Rules is every rule of a policy.
func (c *Client) Rules(ctx context.Context, policyID string) ([]Rule, error) {
	return list[Rule](ctx, c, "/iam/v1alpha1/rules", url.Values{"policy_id": {policyID}}, "rules")
}

// SetRules replaces every rule of a policy, keeping its ID and its principal.
func (c *Client) SetRules(ctx context.Context, policyID string, rules []RuleSpec) error {
	return c.do(ctx, http.MethodPut, "/iam/v1alpha1/rules", nil,
		map[string]any{"policy_id": policyID, "rules": rules}, nil)
}

// DeletePolicy deletes a policy; one already gone is the state asked for.
func (c *Client) DeletePolicy(ctx context.Context, id string) error {
	if err := c.do(ctx, http.MethodDelete, "/iam/v1alpha1/policies/"+url.PathEscape(id), nil, nil, nil); err != nil && !gone(err) {
		return err
	}
	return nil
}

// PermissionSets is the organization's catalogue of permission sets.
func (c *Client) PermissionSets(ctx context.Context) ([]PermissionSet, error) {
	return list[PermissionSet](ctx, c, "/iam/v1alpha1/permission-sets",
		url.Values{"organization_id": {c.org}}, "permission_sets")
}

// Projects is every project of the organization.
func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	return list[Project](ctx, c, "/account/v3/projects", url.Values{"organization_id": {c.org}}, "projects")
}
