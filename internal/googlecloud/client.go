package googlecloud

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/setup"
)

// Google's API hosts. Fixed rather than configurable, so the provisioner's
// token and the federation key's signatures only ever go to Google.
const (
	stsURL            = "https://sts.googleapis.com/v1/token"
	credentialsAPI    = "https://iamcredentials.googleapis.com/v1"
	iamAPI            = "https://iam.googleapis.com/v1"
	resourceManagerV1 = "https://cloudresourcemanager.googleapis.com/v1"
	resourceManagerV3 = "https://cloudresourcemanager.googleapis.com/v3"
	serviceUsageAPI   = "https://serviceusage.googleapis.com/v1"
	cloudAssetAPI     = "https://cloudasset.googleapis.com/v1"
)

// ClientTimeout bounds one call. Every call runs inside a pass with its own
// deadline, so this is slack for a slow answer rather than a budget to spend.
const ClientTimeout = 15 * time.Second

// MaxTokenLifetime is the longest token Google issues by impersonation without
// an organization policy extending it: an hour. The provisioner's token is
// minted for that long and renewed provisionerRenew before it ends.
const (
	MaxTokenLifetime = time.Hour
	provisionerRenew = 5 * time.Minute
)

// maxResponse bounds one answer: a project's policy or a page of services is
// tens of kilobytes, and the cap stops a proxy's page being read whole.
const maxResponse = 8 << 20

// Paging: listings ask for pageSize rows and stop after pageCeiling pages with
// an error, since a short read would read as rows that do not exist.
const (
	pageSize    = 100
	pageCeiling = 100
)

// policyWrites bounds the re-reads of a policy somebody else wrote between
// this client's read and its write (409): a busy policy settles in one or two.
const policyWrites = 5

// APIError is a call Google refused. It holds what Google said, with anything
// shaped like a token taken out, and never the request's own credential.
type APIError struct {
	Method string
	// Path is the host and path called, without the query.
	Path   string
	Status int

	// Code is Google's status (`INVALID_ARGUMENT`, `PERMISSION_DENIED`), or
	// the OAuth error STS answers with (`invalid_grant`); Message its text,
	// and Reasons the details' reasons.
	Code    string
	Message string
	Reasons []string

	// RetryAfter is how long a 429 asked the call to wait, zero otherwise.
	RetryAfter time.Duration
	// Unwaited is why a read handed a 429 back rather than waiting it out.
	Unwaited string
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("googlecloud: %s %s: %s", e.Method, e.Path, e.answer())
	if len(e.Reasons) > 0 {
		s += " [" + strings.Join(e.Reasons, ", ") + "]"
	}
	if e.Unwaited != "" {
		s += fmt.Sprintf(" (asked to wait %s; %s)", e.RetryAfter, e.Unwaited)
	}
	return s
}

// answer is what Google said, without the call it answered: its status,
// its code and its message.
func (e *APIError) answer() string {
	s := strconv.Itoa(e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	return s + ": " + e.Message
}

// Status reports the HTTP status a call was refused with, or 0 when the
// failure was not an API error, so [integration.Reject] can classify it.
func Status(err error) int {
	if api, ok := errors.AsType[*APIError](err); ok {
		return api.Status
	}
	return 0
}

// stage is one step of reaching Google as the provisioner, which a failure
// names so a person knows which half of the setup to look at.
type stage string

const (
	stageFederation    stage = "federation"
	stageImpersonation stage = "impersonation"
)

// stageError is a failure to reach Google as the provisioner at one stage.
type stageError struct {
	stage stage
	err   error
}

func (e *stageError) Error() string { return fmt.Sprintf("googlecloud: %s: %v", e.stage, e.err) }
func (e *stageError) Unwrap() error { return e.err }

// reject is [integration.Reject] for a Google error, and a refused federation
// or impersonation is the provisioner's credential whatever its status: STS
// refuses a token its provider does not trust with a 400.
func reject(err error) error {
	if st, ok := errors.AsType[*stageError](err); ok {
		if s := Status(st.err); s >= http.StatusBadRequest && s < http.StatusInternalServerError &&
			s != http.StatusTooManyRequests {
			return fmt.Errorf("%w: %w", integration.ErrCredentialRejected, err)
		}
	}
	return integration.Reject(err, Status(err))
}

// errUnreached is a call that got no answer from Google: a failure a wait
// clears.
var errUnreached = errors.New("no answer from Google")

// gone reports a read or a delete of something Google no longer has.
func gone(err error) bool { return Status(err) == http.StatusNotFound }

// accountNotYet reports Google refusing a binding because email's account is
// not visible to the policy yet: the 400 a new account answers on another
// project's policy for up to minutes after it was made. Only that account's.
func accountNotYet(err error, email string) bool {
	api, ok := errors.AsType[*APIError](err)
	return ok && email != "" && api.Status == http.StatusBadRequest &&
		strings.Contains(api.Message, email) && strings.Contains(api.Message, "does not exist")
}

// accountQuota reports a create refused because the identity project holds as
// many service accounts as its quota allows.
func accountQuota(err error) bool {
	api, ok := errors.AsType[*APIError](err)
	return ok && (api.Status == http.StatusTooManyRequests || api.Code == "RESOURCE_EXHAUSTED" ||
		strings.Contains(strings.ToLower(api.Message), "maximum number of service accounts"))
}

// domainRestricted reports a grant the organization's domain restricted
// sharing policy refused for the agent's account.
func domainRestricted(err error) bool {
	api, ok := errors.AsType[*APIError](err)
	return ok && (strings.Contains(api.Message, "allowedPolicyMember") ||
		strings.Contains(api.Message, "do not belong to a permitted customer"))
}

// Client talks to Google as the provisioner, through federation.
type Client struct {
	provisioner string
	identity    string
	provider    string
	issuer      string
	subject     string
	key         *ecdsa.PrivateKey
	kid         string
	http        *http.Client
	clock       clock
}

// ClientOptions builds a client.
type ClientOptions struct {
	// Company is the company's name, which the federation token's issuer and
	// subject are made from ([Issuer], [Subject]).
	Company string

	// Provisioner is the provisioner service account's email, whose project
	// is the identity project.
	Provisioner string

	// Provider is the workload identity provider's full resource name.
	Provider string

	// FederationKey is the engine's ES256 private key, PKCS#8 PEM. Never
	// logged.
	FederationKey string `json:"-"`

	// HTTP overrides the transport, for tests.
	HTTP *http.Client
}

// NewClient builds a client, refusing an input that would fail every call
// with a misleading refusal. Each error names the field to change.
func NewClient(opts ClientOptions) (*Client, error) {
	email := strings.TrimSpace(opts.Provisioner)
	identity, ok := config.GoogleCloudServiceAccount(email)
	if !ok {
		return nil, errors.New("googlecloud: integrations.google_cloud.provisioning.provisioner is not a " +
			"service account's email")
	}
	provider := strings.TrimSpace(opts.Provider)
	if !config.GoogleCloudProvider(provider) {
		return nil, errors.New("googlecloud: integrations.google_cloud.provisioning.provider is not a " +
			"workload identity provider's full resource name")
	}
	if org.Slugify(opts.Company) == "" {
		return nil, errors.New("googlecloud: the company's name has no letter or digit to sign its " +
			"federation token as")
	}
	key, err := ParseFederationKey(opts.FederationKey)
	if err != nil {
		return nil, fmt.Errorf("integrations.google_cloud.provisioning.federation_key: %w", err)
	}
	// THE KID IS THE ONE THE CLOUD SHELL BLOCK UPLOADS ([setup.ES256JWKS]),
	// so Google matches every signed token to that key.
	kid, err := setup.ES256KeyID(opts.FederationKey)
	if err != nil {
		return nil, fmt.Errorf("integrations.google_cloud.provisioning.federation_key: %w", err)
	}
	hc := opts.HTTP
	if hc == nil {
		hc = httpx.Client(ClientTimeout)
	}
	return &Client{
		provisioner: email, identity: identity, provider: provider,
		issuer: Issuer(opts.Company), subject: Subject(opts.Company),
		key: key, kid: kid, http: hc, clock: wallClock(),
	}, nil
}

// Provisioner is the provisioner's email.
func (c *Client) Provisioner() string { return c.provisioner }

// AtlasAccount is the atlas's service account: crewlet-atlas in the identity
// project, which the identity block makes where the company asks for it.
func (c *Client) AtlasAccount() string { return AtlasAccountIn(c.identity) }

// IdentityProject is the project the agents' accounts are made in: the
// provisioner's own, which holds nothing else.
func (c *Client) IdentityProject() string { return c.identity }

// member is a service account as a policy binding names it.
func member(email string) string { return "serviceAccount:" + email }

// provisionerTokens is each account's token the engine reaches through the
// federation (the provisioner's, and the atlas's), one per process, connection
// and account, renewed with provisionerRenew to spare. Its map is made at the first
// client's first token, never at init, which Gitlet's push hook pays
// (cmd/crewlet's hookinit_test.go).
//
//nolint:gochecknoglobals // a process-wide credential cache, guarded per entry
var provisionerTokens struct {
	sync.Mutex
	held map[string]*heldToken
}

// heldToken is one cached token. Its mutex is held across a mint, so callers
// racing for an expired token wait on one exchange rather than each making one.
type heldToken struct {
	mu      sync.Mutex
	value   string
	expires time.Time
}

// connection is what a token this client is handed was issued through: the
// provider, the provisioner, the subject and the key, so a changed connection
// never reuses another's token.
func (c *Client) connection() string {
	return strings.Join([]string{c.provider, c.provisioner, c.subject, c.kid}, "|")
}

// held is this client's cache entry for account's token.
func (c *Client) held(account string) *heldToken {
	id := c.connection() + "|" + account
	provisionerTokens.Lock()
	defer provisionerTokens.Unlock()
	h, ok := provisionerTokens.held[id]
	if !ok {
		if provisionerTokens.held == nil {
			provisionerTokens.held = map[string]*heldToken{}
		}
		h = &heldToken{}
		provisionerTokens.held[id] = h
	}
	return h
}

// token is account's token, minted through federation where the cached one is
// missing or due for renewal. Account is the provisioner or the atlas: each is
// impersonated with the federated token itself, so neither reaches the other.
func (c *Client) token(ctx context.Context, account string) (string, error) {
	h := c.held(account)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.value != "" && h.expires.Sub(c.clock.now()) > provisionerRenew {
		return h.value, nil
	}
	fed, err := c.federated(ctx)
	if err != nil {
		return "", &stageError{stage: stageFederation, err: err}
	}
	value, expires, err := c.impersonate(ctx, fed, account, MaxTokenLifetime)
	if err != nil {
		return "", &stageError{stage: stageImpersonation, err: err}
	}
	h.value, h.expires = value, expires
	return value, nil
}

// forget drops a cached token of account's Google refused, so the next call
// mints afresh.
func (c *Client) forget(account, refused string) {
	h := c.held(account)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.value == refused {
		h.value, h.expires = "", time.Time{}
	}
}

// federated signs the engine's token and exchanges it at STS.
func (c *Client) federated(ctx context.Context) (string, error) {
	audience := Audience(c.provider)
	jwt, err := signJWT(c.key, c.kid, c.issuer, c.subject, "https:"+audience, c.clock.now())
	if err != nil {
		return "", err
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"audience":             {audience},
		"scope":                {ScopeCloudPlatform},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"subject_token":        {jwt},
		"subject_token_type":   {"urn:ietf:params:oauth:token-type:jwt"},
	}
	if err := c.do(ctx, http.MethodPost, stsURL, "", form, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("googlecloud: STS answered the exchange with no token")
	}
	return out.AccessToken, nil
}

// impersonate mints an hour or less of account's token with bearer's.
func (c *Client) impersonate(ctx context.Context, bearer, account string, life time.Duration) (string, time.Time, error) {
	var out struct {
		AccessToken string    `json:"accessToken"`
		ExpireTime  time.Time `json:"expireTime"`
	}
	err := c.do(ctx, http.MethodPost,
		credentialsAPI+"/projects/-/serviceAccounts/"+url.PathEscape(account)+":generateAccessToken", bearer,
		map[string]any{"scope": []string{ScopeCloudPlatform}, "lifetime": strconv.Itoa(int(life.Seconds())) + "s"},
		&out)
	if err != nil {
		return "", time.Time{}, err
	}
	if out.AccessToken == "" || out.ExpireTime.IsZero() {
		return "", time.Time{}, errors.New("googlecloud: generateAccessToken answered with no token or expiry")
	}
	return out.AccessToken, out.ExpireTime.UTC(), nil
}

// SeatToken mints an agent's own token: account is its service account's
// email or unique ID, impersonated with the provisioner's token for life, at
// most [MaxTokenLifetime]. The value is never logged; a refusal is an
// [*APIError] whose [Status] an issuer reads (403 or 404 within minutes of a
// grant is Google still applying it).
func (c *Client) SeatToken(ctx context.Context, account string, life time.Duration) (string, time.Time, error) {
	if life <= 0 || life > MaxTokenLifetime {
		return "", time.Time{}, fmt.Errorf("googlecloud: a seat token's lifetime is up to %s, not %s",
			MaxTokenLifetime, life)
	}
	if strings.TrimSpace(account) == "" {
		return "", time.Time{}, errors.New("googlecloud: no service account to mint a seat token for")
	}
	var value string
	var expires time.Time
	err := c.authorized(ctx, func(bearer string) error {
		var err error
		value, expires, err = c.impersonate(ctx, bearer, account, life)
		return err
	})
	return value, expires, err
}

// authorized runs call with the provisioner's token ([Client.authorizedAs]).
func (c *Client) authorized(ctx context.Context, call func(bearer string) error) error {
	return c.authorizedAs(ctx, c.provisioner, call)
}

// authorizedAs runs call with account's token, and once more with a fresh one
// where Google refused the cached token as unauthenticated.
func (c *Client) authorizedAs(ctx context.Context, account string, call func(bearer string) error) error {
	bearer, err := c.token(ctx, account)
	if err != nil {
		return err
	}
	err = call(bearer)
	if Status(err) != http.StatusUnauthorized {
		return err
	}
	c.forget(account, bearer)
	if bearer, err = c.token(ctx, account); err != nil {
		return err
	}
	return call(bearer)
}

// call is one request as the provisioner.
func (c *Client) call(ctx context.Context, method, address string, body, out any) error {
	return c.callAs(ctx, c.provisioner, method, address, body, out)
}

// callAs is one request as account.
func (c *Client) callAs(ctx context.Context, account, method, address string, body, out any) error {
	return c.authorizedAs(ctx, account,
		func(bearer string) error { return c.do(ctx, method, address, bearer, body, out) })
}

// do makes one call, waiting out a short 429 on a read as Google asks. A
// write's 429 is handed back as it came: a create Google refuses for a quota
// answers the same status, and a wait clears no quota.
func (c *Client) do(ctx context.Context, method, address, bearer string, body, out any) error {
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, method, address, bearer, body, out)
		api, ok := errors.AsType[*APIError](err)
		if !ok || api.Status != http.StatusTooManyRequests || method != http.MethodGet {
			return err
		}
		limit := httpx.VendorRateLimit()
		limit.Wait = c.clock.sleep
		unwaited := limit.WaitOut(ctx, attempt, api.RetryAfter)
		if unwaited == nil {
			continue
		}
		api.Unwaited = unwaited.Error()
		return api
	}
}

// once sends one request and decodes its answer into out. It is the only
// place a request is built.
func (c *Client) once(ctx context.Context, method, address, bearer string, body, out any) error {
	var reader io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case url.Values:
		reader, contentType = strings.NewReader(b.Encode()), "application/x-www-form-urlencoded"
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			return fmt.Errorf("googlecloud: encode %s %s: %w", method, pathOf(address), err)
		}
		reader, contentType = bytes.NewReader(encoded), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return fmt.Errorf("googlecloud: build %s %s: %w", method, pathOf(address), err)
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := c.http.Do(req)
	if err != nil {
		// THE TRANSPORT'S TEXT, REDACTED, never its error: it can quote what
		// was sent.
		return fmt.Errorf("googlecloud: %s %s: %w: %s", method, pathOf(address), errUnreached,
			redact.Secrets(err.Error()))
	}
	defer func() { _ = res.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("googlecloud: read %s %s: %w", method, pathOf(address), err)
	}
	if res.StatusCode >= http.StatusMultipleChoices {
		api := refusalOf(method, pathOf(address), res, payload)
		if res.StatusCode == http.StatusTooManyRequests {
			// GOOGLE NAMES A WAIT ON SOME LIMITS AND NONE ON OTHERS.
			wait, named := httpx.RetryAfter(res.Header, c.clock.now())
			if !named {
				wait = httpx.DefaultRetryAfter
			}
			api.RetryAfter = wait
		}
		return api
	}
	if out != nil && len(bytes.TrimSpace(payload)) > 0 {
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("googlecloud: decode %s %s: %w", method, pathOf(address), err)
		}
	}
	return nil
}

// pathOf is an address's host and path, which is all an error names.
func pathOf(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return "a Google API"
	}
	return u.Host + u.Path
}

// refusalOf reads Google's refusal: its RPC status shape, or STS's OAuth one.
func refusalOf(method, path string, res *http.Response, payload []byte) *APIError {
	api := &APIError{Method: method, Path: path, Status: res.StatusCode}
	var envelope struct {
		Error       json.RawMessage `json:"error"`
		Description string          `json:"error_description"`
	}
	if json.Unmarshal(payload, &envelope) == nil && len(envelope.Error) > 0 {
		var rpc struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		}
		var oauth string
		switch {
		case json.Unmarshal(envelope.Error, &rpc) == nil && (rpc.Message != "" || rpc.Status != ""):
			api.Code, api.Message = rpc.Status, rpc.Message
			for _, d := range rpc.Details {
				if d.Reason != "" {
					api.Reasons = append(api.Reasons, d.Reason)
				}
			}
		case json.Unmarshal(envelope.Error, &oauth) == nil:
			api.Code, api.Message = oauth, envelope.Description
		}
	}
	if strings.TrimSpace(api.Message) == "" {
		api.Message = httpx.Refusal(res.Header.Get("Content-Type"), payload)
	}
	if strings.TrimSpace(api.Message) == "" {
		api.Message = "no detail"
	}
	// A REFUSAL NEVER CARRIES A TOKEN ON, and Google's can quote one: STS
	// echoes the federation token it would not trust, a JWT, and a call's
	// refusal can name the access token it was made with. Taken out by the
	// one denylist every surface redacts with (internal/redact), which knows
	// both shapes and every other; a copy of two of them here was how a
	// shape its neighbour learned would never reach this text.
	api.Message = redact.Secrets(api.Message)
	return api
}

// ServiceAccount is one service account, as Google reports it.
type ServiceAccount struct {
	Name        string `json:"name"`
	ProjectID   string `json:"projectId"`
	UniqueID    string `json:"uniqueId"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Disabled    bool   `json:"disabled"`
}

// accountURL is one account of the identity project, by email or unique ID.
func (c *Client) accountURL(account string) string {
	return iamAPI + "/projects/" + url.PathEscape(c.identity) + "/serviceAccounts/" + url.PathEscape(account)
}

// ServiceAccount reads one account of the identity project by its email or
// unique ID, and false where Google has none.
func (c *Client) ServiceAccount(ctx context.Context, account string) (ServiceAccount, bool, error) {
	var sa ServiceAccount
	err := c.call(ctx, http.MethodGet, c.accountURL(account), nil, &sa)
	switch {
	case gone(err):
		return ServiceAccount{}, false, nil
	case err != nil:
		return ServiceAccount{}, false, err
	}
	return sa, true, nil
}

// ServiceAccounts is every account of the identity project.
func (c *Client) ServiceAccounts(ctx context.Context) ([]ServiceAccount, error) {
	base := iamAPI + "/projects/" + url.PathEscape(c.identity) + "/serviceAccounts"
	return pages(ctx, c, base, url.Values{}, func(page json.RawMessage) ([]ServiceAccount, string, error) {
		var resp struct {
			Accounts []ServiceAccount `json:"accounts"`
			Next     string           `json:"nextPageToken"`
		}
		err := json.Unmarshal(page, &resp)
		return resp.Accounts, resp.Next, err
	})
}

// CreateServiceAccount makes an account in the identity project.
func (c *Client) CreateServiceAccount(ctx context.Context, id, displayName, description string) (ServiceAccount, error) {
	var sa ServiceAccount
	err := c.call(ctx, http.MethodPost, iamAPI+"/projects/"+url.PathEscape(c.identity)+"/serviceAccounts",
		map[string]any{"accountId": id, "serviceAccount": map[string]string{
			"displayName": displayName, "description": description,
		}}, &sa)
	if err == nil && (sa.UniqueID == "" || sa.Email == "") {
		return ServiceAccount{}, errors.New("googlecloud: the create answered with no unique ID or email")
	}
	return sa, err
}

// DisableServiceAccount turns an account off: Google refuses every token it
// holds from then on.
func (c *Client) DisableServiceAccount(ctx context.Context, account string) error {
	return c.call(ctx, http.MethodPost, c.accountURL(account)+":disable", map[string]any{}, nil)
}

// EnableServiceAccount turns an account back on, with its roles as they were.
func (c *Client) EnableServiceAccount(ctx context.Context, account string) error {
	return c.call(ctx, http.MethodPost, c.accountURL(account)+":enable", map[string]any{}, nil)
}

// DeleteServiceAccount deletes an account; one already gone is the state
// asked for.
func (c *Client) DeleteServiceAccount(ctx context.Context, account string) error {
	if err := c.call(ctx, http.MethodDelete, c.accountURL(account), nil, nil); err != nil && !gone(err) {
		return err
	}
	return nil
}

// AccountPolicy is the allow policy on one account of the identity project:
// who may act as it.
func (c *Client) AccountPolicy(ctx context.Context, account string) (Policy, error) {
	var pol Policy
	err := c.call(ctx, http.MethodPost, c.accountURL(account)+":getIamPolicy", map[string]any{}, &pol)
	return pol, err
}

// EditAccountPolicy applies edit to an account's policy, re-reading where
// somebody else wrote it in between, and writes only where edit changed it.
// The version is written back as read, so a condition on it survives.
func (c *Client) EditAccountPolicy(ctx context.Context, account string, edit func(*Policy) bool) error {
	return c.editPolicy(ctx, account, func(ctx context.Context) (Policy, error) {
		return c.AccountPolicy(ctx, account)
	}, func(ctx context.Context, pol Policy) error {
		return c.call(ctx, http.MethodPost, c.accountURL(account)+":setIamPolicy", map[string]any{"policy": pol}, nil)
	}, edit)
}

// ProjectPolicy is one project's allow policy, at version 3 so its
// conditions, the provisioner's own included, are read whole.
func (c *Client) ProjectPolicy(ctx context.Context, project string) (Policy, error) {
	var pol Policy
	err := c.call(ctx, http.MethodPost, resourceManagerV1+"/projects/"+url.PathEscape(project)+":getIamPolicy",
		map[string]any{"options": map[string]int{"requestedPolicyVersion": 3}}, &pol)
	return pol, err
}

// EditProjectPolicy applies edit to a project's policy, re-reading where
// somebody else wrote it in between, and writes only where edit changed it.
//
// ALWAYS VERSION 3 WITH THE ETAG: a write that omits the version may replace a
// version 3 policy with version 1, dropping every condition, the
// provisioner's own containment included.
func (c *Client) EditProjectPolicy(ctx context.Context, project string, edit func(*Policy) bool) error {
	return c.editPolicy(ctx, project, func(ctx context.Context) (Policy, error) {
		return c.ProjectPolicy(ctx, project)
	}, func(ctx context.Context, pol Policy) error {
		pol.Version = 3
		return c.call(ctx, http.MethodPost, resourceManagerV1+"/projects/"+url.PathEscape(project)+":setIamPolicy",
			map[string]any{"policy": pol}, nil)
	}, edit)
}

// editPolicy is one read-modify-write of a policy, retried on 409.
func (c *Client) editPolicy(
	ctx context.Context, resource string,
	read func(context.Context) (Policy, error), write func(context.Context, Policy) error, edit func(*Policy) bool,
) error {
	for range policyWrites {
		pol, err := read(ctx)
		if err != nil {
			return err
		}
		if !edit(&pol) {
			return nil
		}
		err = write(ctx, pol)
		if Status(err) == http.StatusConflict {
			continue
		}
		return err
	}
	return fmt.Errorf("googlecloud: the policy of %s kept changing under %d writes", resource, policyWrites)
}

// TestProjectPermissions is which of perms the provisioner holds on project.
func (c *Client) TestProjectPermissions(ctx context.Context, project string, perms []string) ([]string, error) {
	var out struct {
		Permissions []string `json:"permissions"`
	}
	err := c.call(ctx, http.MethodPost, resourceManagerV1+"/projects/"+url.PathEscape(project)+":testIamPermissions",
		map[string]any{"permissions": perms}, &out)
	return out.Permissions, err
}

// Project is one project the provisioner can read.
type Project struct {
	ProjectID   string `json:"projectId"`
	DisplayName string `json:"displayName"`
	State       string `json:"state"`
	// Parent is `organizations/<id>` or `folders/<id>`, empty for a project
	// in no organization.
	Parent string `json:"parent"`
}

// SearchProjects is every project the provisioner can read, active or not.
func (c *Client) SearchProjects(ctx context.Context) ([]Project, error) {
	return pages(ctx, c, resourceManagerV3+"/projects:search", url.Values{},
		func(page json.RawMessage) ([]Project, string, error) {
			var resp struct {
				Projects []Project `json:"projects"`
				Next     string    `json:"nextPageToken"`
			}
			err := json.Unmarshal(page, &resp)
			return resp.Projects, resp.Next, err
		})
}

// Role is one IAM role, predefined or a project's custom role.
type Role struct {
	Name        string   `json:"name"`
	Title       string   `json:"title"`
	Stage       string   `json:"stage"`
	Permissions []string `json:"includedPermissions"`
	Deleted     bool     `json:"deleted"`
}

// Role reads one role by its name (`roles/browser`,
// `projects/<id>/roles/crewletOperator`), and false where Google has none.
func (c *Client) Role(ctx context.Context, name string) (Role, bool, error) {
	var r Role
	err := c.call(ctx, http.MethodGet, iamAPI+"/"+name, nil, &r)
	switch {
	case gone(err):
		return Role{}, false, nil
	case err != nil:
		return Role{}, false, err
	}
	return r, true, nil
}

// EnabledServices is every API enabled on project, by its service name.
func (c *Client) EnabledServices(ctx context.Context, project string) (map[string]bool, error) {
	rows, err := pages(ctx, c, serviceUsageAPI+"/projects/"+url.PathEscape(project)+"/services",
		url.Values{"filter": {"state:ENABLED"}}, func(page json.RawMessage) ([]string, string, error) {
			var resp struct {
				Services []struct {
					Config struct {
						Name string `json:"name"`
					} `json:"config"`
				} `json:"services"`
				Next string `json:"nextPageToken"`
			}
			if err := json.Unmarshal(page, &resp); err != nil {
				return nil, "", err
			}
			names := make([]string, 0, len(resp.Services))
			for _, s := range resp.Services {
				names = append(names, s.Config.Name)
			}
			return names, resp.Next, nil
		})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, name := range rows {
		out[name] = true
	}
	return out, nil
}

// Asset is one resource Cloud Asset Inventory lists, as the atlas keeps it:
// its full resource name, its type, its name, where it is, its state, and
// for an instance its addresses. Nothing else of the answer is decoded.
type Asset struct {
	Name        string
	Type        string
	DisplayName string
	Location    string
	State       string
	Addresses   []string
}

// assetReadMask is every field a search asks for. THE MASK IS EXPLICIT AND
// CLOSED: unset, Google answers labels, tags and descriptions, and with
// versionedResources a resource's whole configuration, which for a function
// or a service holds its environment.
const assetReadMask = "name,assetType,displayName,location,state,additionalAttributes"

// assetPageSize is the most results Google answers a search page with.
const assetPageSize = 500

// SearchAssets is every resource of assetTypes in project, searched as the
// atlas's account. A project the account may not search is a 403.
func (c *Client) SearchAssets(ctx context.Context, project string, assetTypes []string) ([]Asset, error) {
	query := url.Values{"assetTypes": assetTypes, "readMask": {assetReadMask}}
	return pagesAs(ctx, c, c.AtlasAccount(), cloudAssetAPI+"/projects/"+url.PathEscape(project)+":searchAllResources",
		query, assetPageSize, func(page json.RawMessage) ([]Asset, string, error) {
			var resp struct {
				Results []struct {
					Name        string `json:"name"`
					AssetType   string `json:"assetType"`
					DisplayName string `json:"displayName"`
					Location    string `json:"location"`
					State       string `json:"state"`
					// ONLY TWO ATTRIBUTES ARE DECODED, and only as lists of
					// text: an instance's internal and external addresses.
					Attributes struct {
						InternalIPs []string `json:"internalIPs"`
						ExternalIPs []string `json:"externalIPs"`
					} `json:"additionalAttributes"`
				} `json:"results"`
				Next string `json:"nextPageToken"`
			}
			if err := json.Unmarshal(page, &resp); err != nil {
				return nil, "", err
			}
			out := make([]Asset, 0, len(resp.Results))
			for _, r := range resp.Results {
				a := Asset{Name: r.Name, Type: r.AssetType, DisplayName: r.DisplayName, Location: r.Location,
					State: r.State}
				if r.AssetType == AssetInstance {
					a.Addresses = append(append(a.Addresses, r.Attributes.InternalIPs...), r.Attributes.ExternalIPs...)
				}
				out = append(out, a)
			}
			return out, resp.Next, nil
		})
}

// pages reads a paged listing to its end, each page decoded by decode into its
// rows and the next page's token.
func pages[T any](
	ctx context.Context, c *Client, base string, query url.Values,
	decode func(json.RawMessage) ([]T, string, error),
) ([]T, error) {
	return pagesAs(ctx, c, c.provisioner, base, query, pageSize, decode)
}

// pagesAs is [pages] as account, size rows a page.
func pagesAs[T any](
	ctx context.Context, c *Client, account, base string, query url.Values, size int,
	decode func(json.RawMessage) ([]T, string, error),
) ([]T, error) {
	var out []T
	next := ""
	for page := 0; ; page++ {
		if page == pageCeiling {
			return nil, fmt.Errorf("googlecloud: %s did not finish after %d pages", pathOf(base), pageCeiling)
		}
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("pageSize", strconv.Itoa(size))
		if next != "" {
			q.Set("pageToken", next)
		}
		var raw json.RawMessage
		if err := c.callAs(ctx, account, http.MethodGet, base+"?"+q.Encode(), nil, &raw); err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			// AN EMPTY ANSWER IS AN EMPTY LISTING, which Google gives
			// a project with no row as `{}` or nothing at all.
			return out, nil
		}
		rows, token, err := decode(raw)
		if err != nil {
			return nil, fmt.Errorf("googlecloud: decode %s: %w", pathOf(base), err)
		}
		out = append(out, rows...)
		if token == "" {
			return out, nil
		}
		next = token
	}
}
