package googleauth

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
	"slices"
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

// Connection is the company's Google identity: the provisioner and any other
// account of the identity project the company's federated subject may act as,
// reached with no service account key.
type Connection struct {
	provisioner string
	identity    string
	provider    string
	issuer      string
	subject     string
	key         *ecdsa.PrivateKey
	kid         string
	Transport
}

// Transport is the one way a request reaches Google ([Transport.Do]): a
// connection's, and a consent product's, which sends a person's grant's token
// on it with no Google identity behind it.
type Transport struct {
	http  *http.Client
	clock Clock
}

// Raw is an answer kept as Google sent it: a method answering
// google.api.HttpBody (a CSV report) sends bytes of their own type, not JSON.
type Raw struct {
	ContentType string
	Data        []byte
}

// NewTransport is Google's call path on hc, nil for the engine's own.
func NewTransport(hc *http.Client) *Transport {
	if hc == nil {
		hc = httpx.Client(ClientTimeout)
	}
	return &Transport{http: hc, clock: WallClock()}
}

// Options builds a connection.
type Options struct {
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

// New builds a connection, refusing an input that would fail every call with
// a misleading refusal as a [*ConfigError] naming the field to change.
func New(opts Options) (*Connection, error) {
	email := strings.TrimSpace(opts.Provisioner)
	identity, ok := config.GoogleCloudServiceAccount(email)
	if !ok {
		return nil, &ConfigError{Sentence: "the Identity project ID names no project a service account can be made in"}
	}
	provider := strings.TrimSpace(opts.Provider)
	if !config.GoogleCloudProvider(provider) {
		return nil, &ConfigError{Sentence: "the Workload identity provider is not a provider's full resource name"}
	}
	if org.Slugify(opts.Company) == "" {
		return nil, &ConfigError{Sentence: "the company's name has no letter or digit to sign its federation token as"}
	}
	key, problem := parseFederationKey(opts.FederationKey)
	if problem != "" {
		return nil, &ConfigError{Sentence: problem}
	}
	// THE KID IS THE ONE THE CLOUD SHELL BLOCK UPLOADS ([setup.ES256KeyID]),
	// so Google matches every signed token to that key.
	kid, err := setup.ES256KeyID(opts.FederationKey)
	if err != nil {
		return nil, &ConfigError{Sentence: "the Federation key is not one Crewlet can sign with"}
	}
	return &Connection{
		provisioner: email, identity: identity, provider: provider,
		issuer: Issuer(opts.Company), subject: Subject(opts.Company),
		key: key, kid: kid, Transport: *NewTransport(opts.HTTP),
	}, nil
}

// fieldLabels are the setup form's names for the connection's fields, which a
// sentence names a field by (googlecloud.Requirements).
var fieldLabels = map[string]string{"identity_project": "Identity project ID",
	"provider": "Workload identity provider", "federation_key": "Federation key"}

// For is the connection of company's Google identity, from its document as
// env resolves it. An error wrapping [integration.ErrNotConfigured] is a
// company without the identity (Google Cloud off or no provisioning block),
// or one whose values do not resolve.
func For(c *config.Company, env *config.Resolver, hc *http.Client) (*Connection, error) {
	if c == nil || c.Integrations.GoogleCloud == nil || !c.Integrations.GoogleCloud.Enabled ||
		c.Integrations.GoogleCloud.Provisioning == nil {
		return nil, &ConfigError{Sentence: "Google Cloud is not set up in Crewlet",
			Err: integration.ErrNotConfigured}
	}
	if env == nil {
		return nil, &ConfigError{Sentence: "the Federation key could not be read", Err: integration.ErrNotConfigured}
	}
	prov := c.Integrations.GoogleCloud.Provisioning
	values := map[string]string{
		"identity_project": strings.TrimSpace(env.Value(prov.IdentityProject)),
		"provider":         strings.TrimSpace(env.Value(prov.Provider)),
		"federation_key":   strings.TrimSpace(env.Value(prov.FederationKey)),
	}
	for _, field := range []string{"identity_project", "provider", "federation_key"} {
		if values[field] == "" {
			return nil, &ConfigError{Sentence: "the " + fieldLabels[field] + " is not set yet",
				Err: integration.ErrNotConfigured}
		}
	}
	if !config.GoogleCloudIdentityProject(values["identity_project"]) {
		return nil, &ConfigError{Sentence: "the Identity project ID is not a Google Cloud project ID"}
	}
	return New(Options{
		Company: c.Name, Provisioner: config.GoogleCloudProvisioner(values["identity_project"]),
		Provider: values["provider"], FederationKey: values["federation_key"], HTTP: hc,
	})
}

// WithClock is the same connection reading and waiting by clk. Its tokens are
// the same: the caches are keyed by [Connection.ID], never by the value.
func (c *Connection) WithClock(clk Clock) *Connection {
	out := *c
	out.clock = clk
	return &out
}

// Clock is the time this connection reads and waits by.
func (c *Connection) Clock() Clock { return c.clock }

// Provisioner is the provisioner's email.
func (c *Connection) Provisioner() string { return c.provisioner }

// IdentityProject is the project the company's Google identities are made in:
// the provisioner's own, which holds nothing else.
func (c *Connection) IdentityProject() string { return c.identity }

// Provider is the workload identity provider's full resource name.
func (c *Connection) Provider() string { return c.provider }

// ID is what a token this connection is handed was issued through: the
// provider, the provisioner, the subject and the key, so a changed connection
// never reuses another's token.
func (c *Connection) ID() string {
	return strings.Join([]string{c.provider, c.provisioner, c.subject, c.kid}, "|")
}

// federatedTokens is each account's token the engine reaches through the
// federation (the provisioner's, the atlas's), one per process, connection,
// account and scope set, renewed with federatedRenew to spare. Its map is made
// at the first connection's first token, never at init, which Gitlet's push
// hook pays (cmd/crewlet's hookinit_test.go).
//
//nolint:gochecknoglobals // a process-wide credential cache, guarded per entry
var federatedTokens struct {
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

// scopeKey is a scope set as one key: sorted, so the order a caller lists
// them in never splits one token into two. One scope, the common case, is its
// own key.
func scopeKey(scopes []string) string {
	if len(scopes) == 1 {
		return scopes[0]
	}
	sorted := slices.Clone(scopes)
	slices.Sort(sorted)
	return strings.Join(slices.Compact(sorted), " ")
}

// held is this connection's cache entry for account's token with the scope
// set whose [scopeKey] is scopes.
func (c *Connection) held(account, scopes string) *heldToken {
	id := c.ID() + "|" + account + "|" + scopes
	federatedTokens.Lock()
	defer federatedTokens.Unlock()
	h, ok := federatedTokens.held[id]
	if !ok {
		if federatedTokens.held == nil {
			federatedTokens.held = map[string]*heldToken{}
		}
		h = &heldToken{}
		federatedTokens.held[id] = h
	}
	return h
}

// FederatedToken is account's token with scopes, minted through federation
// where the cached one is missing or due for renewal. Account is one the
// company's subject impersonates directly (the provisioner, the atlas), so
// none of them reaches another.
func (c *Connection) FederatedToken(ctx context.Context, account string, scopes ...string) (string, error) {
	key := ScopeCloudPlatform
	if len(scopes) > 0 {
		key = scopeKey(scopes)
	} else {
		scopes = []string{ScopeCloudPlatform}
	}
	h := c.held(account, key)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.value != "" && h.expires.Sub(c.clock.Now()) > federatedRenew {
		return h.value, nil
	}
	fed, err := c.federated(ctx)
	if err != nil {
		return "", &StageError{Stage: StageFederation, Err: err}
	}
	value, expires, err := c.impersonate(ctx, fed, account, scopes, MaxTokenLifetime)
	if err != nil {
		return "", &StageError{Stage: StageImpersonation, Err: err}
	}
	h.value, h.expires = value, expires
	return value, nil
}

// forget drops a cached token of account's Google refused, so the next call
// mints afresh.
func (c *Connection) forget(account, scopes, refused string) {
	h := c.held(account, scopes)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.value == refused {
		h.value, h.expires = "", time.Time{}
	}
}

// federated signs the engine's token and exchanges it at STS. THE EXCHANGE IS
// ALWAYS CLOUD-PLATFORM: only the impersonation names a product's scopes.
func (c *Connection) federated(ctx context.Context) (string, error) {
	audience := Audience(c.provider)
	jwt, err := signJWT(c.key, c.kid, c.issuer, c.subject, "https:"+audience, c.clock.Now())
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
	if err := c.Do(ctx, http.MethodPost, stsURL, "", form, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("googleauth: STS answered the exchange with no token")
	}
	return out.AccessToken, nil
}

// impersonate mints an hour or less of account's token with scopes, using
// bearer's.
func (c *Connection) impersonate(
	ctx context.Context, bearer, account string, scopes []string, life time.Duration,
) (string, time.Time, error) {
	var out struct {
		AccessToken string    `json:"accessToken"`
		ExpireTime  time.Time `json:"expireTime"`
	}
	err := c.Do(ctx, http.MethodPost,
		credentialsAPI+"/projects/-/serviceAccounts/"+url.PathEscape(account)+":generateAccessToken", bearer,
		map[string]any{"scope": scopes, "lifetime": strconv.Itoa(int(life.Seconds())) + "s"}, &out)
	if err != nil {
		return "", time.Time{}, err
	}
	if out.AccessToken == "" || out.ExpireTime.IsZero() {
		return "", time.Time{}, errors.New("googleauth: generateAccessToken answered with no token or expiry")
	}
	return out.AccessToken, out.ExpireTime.UTC(), nil
}

// SeatToken mints a token of account with scopes: account is a service
// account's email or unique ID the provisioner may mint for, impersonated with
// the provisioner's token for life, at most [MaxTokenLifetime]. The value is
// never logged; a refusal is an [*APIError] whose [Status] an issuer reads.
func (c *Connection) SeatToken(
	ctx context.Context, account string, scopes []string, life time.Duration,
) (string, time.Time, error) {
	if life <= 0 || life > MaxTokenLifetime {
		return "", time.Time{}, fmt.Errorf("googleauth: a seat token's lifetime is up to %s, not %s",
			MaxTokenLifetime, life)
	}
	if strings.TrimSpace(account) == "" {
		return "", time.Time{}, errors.New("googleauth: no service account to mint a seat token for")
	}
	if len(scopes) == 0 {
		return "", time.Time{}, errors.New("googleauth: a seat token names the scopes it is minted with")
	}
	var value string
	var expires time.Time
	err := c.AuthorizedAs(ctx, c.provisioner, func(bearer string) error {
		var err error
		value, expires, err = c.impersonate(ctx, bearer, account, scopes, life)
		return err
	})
	return value, expires, err
}

// AuthorizedAs runs call with federated account's cloud-platform token, and
// once more with a fresh one where Google refused the cached token as
// unauthenticated.
func (c *Connection) AuthorizedAs(ctx context.Context, account string, call func(bearer string) error) error {
	bearer, err := c.FederatedToken(ctx, account)
	if err != nil {
		return err
	}
	err = call(bearer)
	if Status(err) != http.StatusUnauthorized {
		return err
	}
	c.forget(account, ScopeCloudPlatform, bearer)
	if bearer, err = c.FederatedToken(ctx, account); err != nil {
		return err
	}
	return call(bearer)
}

// CallAs is one request as federated account.
func (c *Connection) CallAs(ctx context.Context, account, method, address string, body, out any) error {
	return c.AuthorizedAs(ctx, account,
		func(bearer string) error { return c.Do(ctx, method, address, bearer, body, out) })
}

// Do makes one call with bearer (none for ""), waiting out a short 429 on a
// read as Google asks. A write's 429 is handed back as it came: a create
// Google refuses for a quota answers the same status, and a wait clears no
// quota.
func (c *Transport) Do(ctx context.Context, method, address, bearer string, body, out any) error {
	return c.Send(ctx, Call{Method: method, Address: address, Bearer: bearer, Body: body,
		Reads: method == http.MethodGet}, out)
}

// Call is one request to Google: its method, address, bearer (none for ""),
// the headers its API asks beyond the engine's own (Google Ads' manager), its
// body, and whether it changes nothing, which waits out a short 429.
type Call struct {
	Method, Address, Bearer string
	Header                  http.Header
	Body                    any
	Reads                   bool
}

// Bytes is a call's body sent as it is, such as a file an upload sends. An
// answer read into an io.Writer is streamed to it as it came (a download).
type Bytes struct {
	ContentType string
	Data        []byte
}

// Send makes call, waiting out a short 429 where it reads.
func (c *Transport) Send(ctx context.Context, call Call, out any) error {
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, call, out)
		api, ok := errors.AsType[*APIError](err)
		if !ok || api.Status != http.StatusTooManyRequests || !call.Reads {
			return err
		}
		limit := httpx.VendorRateLimit()
		limit.Wait = c.clock.Sleep
		unwaited := limit.WaitOut(ctx, attempt, api.RetryAfter)
		if unwaited == nil {
			continue
		}
		api.Unwaited = unwaited.Error()
		return api
	}
}

// DoRead is [Transport.Do] for a call that changes nothing whatever its
// method (a report or a query taken in a POST body): its 429 is waited out
// as a GET's is.
func (c *Transport) DoRead(ctx context.Context, method, address, bearer string, body, out any) error {
	return c.Send(ctx, Call{Method: method, Address: address, Bearer: bearer, Body: body, Reads: true}, out)
}

// once sends one request and decodes its answer into out, or keeps it as
// sent where out is a [*Raw]. It is the only place a request to Google is
// built; the engine's own headers are set last, so no API's header stands in
// for them.
func (c *Transport) once(ctx context.Context, call Call, out any) error {
	method, address, bearer, body := call.Method, call.Address, call.Bearer, call.Body
	var reader io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case Bytes:
		reader, contentType = bytes.NewReader(b.Data), b.ContentType
	case url.Values:
		reader, contentType = strings.NewReader(b.Encode()), "application/x-www-form-urlencoded"
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			return fmt.Errorf("googleauth: encode %s %s: %w: %w", method, PathOf(address), ErrUnsent, err)
		}
		reader, contentType = bytes.NewReader(encoded), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return fmt.Errorf("googleauth: build %s %s: %w: %w", method, PathOf(address), ErrUnsent,
			pathOnly(err, address))
	}
	raw, bytesAnswer := out.(*Raw)
	for name, values := range call.Header {
		req.Header[http.CanonicalHeaderKey(name)] = values
	}
	req.Header.Set("Accept", "application/json")
	if bytesAnswer {
		req.Header.Set("Accept", "*/*")
	}
	req.Header.Del("Authorization")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	// A CONTEXT ENDED BEFORE THE SEND sent nothing; one that ends during it
	// may have, so only this side of Do is unsent.
	if ended := ctx.Err(); ended != nil {
		return fmt.Errorf("googleauth: %s %s: %w: %w", method, PathOf(address), ErrUnsent, ended)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("googleauth: %s %s: %w: %w", method, PathOf(address), ErrUnreached, pathOnly(err, address))
	}
	defer func() { _ = res.Body.Close() }()
	if w, ok := out.(io.Writer); ok && res.StatusCode < http.StatusMultipleChoices {
		// A FILE IS STREAMED to its writer, which bounds what it keeps.
		if _, copyErr := io.Copy(w, res.Body); copyErr != nil {
			return fmt.Errorf("googleauth: read %s %s: %w", method, PathOf(address), copyErr)
		}
		return nil
	}
	payload, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("googleauth: read %s %s: %w", method, PathOf(address), err)
	}
	if res.StatusCode >= http.StatusMultipleChoices {
		api := refusalOf(method, PathOf(address), res, payload)
		if res.StatusCode == http.StatusTooManyRequests {
			// GOOGLE NAMES A WAIT ON SOME LIMITS AND NONE ON OTHERS.
			wait, named := httpx.RetryAfter(res.Header, c.clock.Now())
			if !named {
				wait = httpx.DefaultRetryAfter
			}
			api.RetryAfter = wait
		}
		return api
	}
	if len(payload) > maxResponse {
		// A CUT ANSWER IS NO ANSWER: cut bytes would read as a whole report,
		// and cut JSON as Google's broken answer.
		return fmt.Errorf("googleauth: %s %s: %w", method, PathOf(address), ErrTooLarge)
	}
	if bytesAnswer {
		raw.ContentType, raw.Data = res.Header.Get("Content-Type"), payload
		return nil
	}
	if out != nil && len(bytes.TrimSpace(payload)) > 0 {
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("googleauth: decode %s %s: %w", method, PathOf(address), err)
		}
	}
	return nil
}

// transportError is a failure to send a request as an error may carry it on:
// its text redacted, and its cause kept, so a deadline or a timeout is told.
type transportError struct{ err error }

func (e transportError) Error() string { return redact.Secrets(e.err.Error()) }
func (e transportError) Unwrap() error { return e.err }

// pathOnly is err, a request's failure, naming address's host and path alone:
// a *url.Error quotes the whole address, whose query can hold a credential (a
// Measurement Protocol secret), and no redaction knows every such name.
func pathOnly(err error, address string) error {
	if u, ok := errors.AsType[*url.Error](err); ok {
		return transportError{&url.Error{Op: u.Op, URL: PathOf(address), Err: u.Err}}
	}
	return transportError{err}
}

// PathOf is an address's host and path, which is all an error names.
func PathOf(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return "a Google API"
	}
	return u.Host + u.Path
}
